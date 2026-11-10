package probe

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func boolPtr(b bool) *bool    { return &b }
func strPtr(s string) *string { return &s }
func reclaimPtr(p corev1.PersistentVolumeReclaimPolicy) *corev1.PersistentVolumeReclaimPolicy {
	return &p
}
func bindingPtr(m storagev1.VolumeBindingMode) *storagev1.VolumeBindingMode { return &m }

func TestStorageClassHelpers(t *testing.T) {
	plain := &storagev1.StorageClass{}
	if reclaimPolicy(plain) != "" || bindingMode(plain) != "" {
		t.Error("nil pointers must yield empty strings")
	}
	full := &storagev1.StorageClass{
		ReclaimPolicy:     reclaimPtr(corev1.PersistentVolumeReclaimDelete),
		VolumeBindingMode: bindingPtr(storagev1.VolumeBindingWaitForFirstConsumer),
	}
	if reclaimPolicy(full) != "Delete" {
		t.Errorf("reclaimPolicy = %q", reclaimPolicy(full))
	}
	if bindingMode(full) != "WaitForFirstConsumer" {
		t.Errorf("bindingMode = %q", bindingMode(full))
	}
	pv := &corev1.PersistentVolume{Spec: corev1.PersistentVolumeSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{
			corev1.ReadWriteOnce, corev1.ReadWriteMany,
		},
	}}
	got := accessModes(pv)
	if len(got) != 2 || got[0] != "ReadWriteOnce" || got[1] != "ReadWriteMany" {
		t.Errorf("accessModes = %v", got)
	}
	if accessModes(&corev1.PersistentVolume{}) != nil {
		t.Error("no access modes should be nil")
	}
}

// seedStorage loads one default and one plain StorageClass, a bound and a
// free PV, and a classed and an unclass PVC.
func seedStorage(t *testing.T, client *fake.Clientset) {
	t.Helper()
	scs := []storagev1.StorageClass{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "slow",
				Annotations: map[string]string{
					// Deliberately not "true": not the default.
					"storageclass.kubernetes.io/is-default-class": "false",
				},
			},
			Provisioner: "slow.example.com",
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name: "fast",
				Annotations: map[string]string{
					"storageclass.kubernetes.io/is-default-class": "true",
				},
			},
			Provisioner:          "pd.csi.example.com",
			ReclaimPolicy:        reclaimPtr(corev1.PersistentVolumeReclaimDelete),
			VolumeBindingMode:    bindingPtr(storagev1.VolumeBindingWaitForFirstConsumer),
			AllowVolumeExpansion: boolPtr(true),
		},
	}
	for i := range scs {
		if _, err := client.StorageV1().StorageClasses().Create(context.Background(), &scs[i], metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	pvs := []corev1.PersistentVolume{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "pv-b"},
			Spec: corev1.PersistentVolumeSpec{
				Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
				AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce, corev1.ReadWriteMany},
				PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
				StorageClassName:              "fast",
				ClaimRef:                      &corev1.ObjectReference{Namespace: "team-a", Name: "data"},
			},
			Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeBound},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "pv-a"},
			Spec: corev1.PersistentVolumeSpec{
				Capacity:    corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("5Gi")},
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			},
			Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeAvailable},
		},
	}
	for i := range pvs {
		if _, err := client.CoreV1().PersistentVolumes().Create(context.Background(), &pvs[i], metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	pvcs := []corev1.PersistentVolumeClaim{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "abc", Namespace: "team-a"},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				},
			},
			Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "data", Namespace: "team-a"},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("5Gi")},
				},
				StorageClassName: strPtr("fast"),
				VolumeName:       "pv-b",
			},
			Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
		},
	}
	for i := range pvcs {
		if _, err := client.CoreV1().PersistentVolumeClaims("team-a").Create(context.Background(), &pvcs[i], metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProbeStorageSuccess(t *testing.T) {
	client := fake.NewClientset()
	seedStorage(t, client)
	p := &prober{in: Input{Client: client, Namespace: "team-a"}}
	info, errs := p.probeStorage(context.Background())
	if len(errs) != 0 {
		t.Fatalf("errs = %v, want none", errs)
	}

	if len(info.StorageClasses) != 2 ||
		info.StorageClasses[0].Name != "fast" || info.StorageClasses[1].Name != "slow" {
		t.Fatalf("storage classes not sorted: %+v", info.StorageClasses)
	}
	fast := info.StorageClasses[0]
	if fast.Provisioner != "pd.csi.example.com" || fast.ReclaimPolicy != "Delete" ||
		fast.VolumeBindingMode != "WaitForFirstConsumer" || !fast.AllowVolumeExpansion || !fast.IsDefault {
		t.Errorf("fast = %+v", fast)
	}
	slow := info.StorageClasses[1]
	if slow.ReclaimPolicy != "" || slow.VolumeBindingMode != "" ||
		slow.AllowVolumeExpansion || slow.IsDefault {
		t.Errorf("slow = %+v, want empty/zero fields", slow)
	}

	if len(info.PersistentVolumes) != 2 ||
		info.PersistentVolumes[0].Name != "pv-a" || info.PersistentVolumes[1].Name != "pv-b" {
		t.Fatalf("PVs not sorted: %+v", info.PersistentVolumes)
	}
	pvB := info.PersistentVolumes[1]
	if pvB.Status != "Bound" || pvB.ReclaimPolicy != "Retain" ||
		pvB.StorageClass != "fast" || pvB.ClaimRef != "team-a/data" ||
		len(pvB.AccessModes) != 2 {
		t.Errorf("pv-b = %+v", pvB)
	}
	if store, ok := pvB.Capacity[corev1.ResourceStorage]; !ok || store.Cmp(resource.MustParse("10Gi")) != 0 {
		t.Errorf("pv-b capacity = %v", pvB.Capacity)
	}
	pvA := info.PersistentVolumes[0]
	if pvA.Status != "Available" || pvA.ClaimRef != "" || pvA.ReclaimPolicy != "" {
		t.Errorf("pv-a = %+v", pvA)
	}

	if len(info.PersistentVolumeClaims) != 2 ||
		info.PersistentVolumeClaims[0].Name != "abc" || info.PersistentVolumeClaims[1].Name != "data" {
		t.Fatalf("PVCs not sorted: %+v", info.PersistentVolumeClaims)
	}
	data := info.PersistentVolumeClaims[1]
	if data.Phase != "Bound" || data.StorageClass != "fast" || data.VolumeName != "pv-b" {
		t.Errorf("data pvc = %+v", data)
	}
	if req, ok := data.Requested[corev1.ResourceStorage]; !ok || req.Cmp(resource.MustParse("5Gi")) != 0 {
		t.Errorf("data requested = %v", data.Requested)
	}
	abc := info.PersistentVolumeClaims[0]
	if abc.StorageClass != "" || abc.Phase != "Pending" {
		t.Errorf("abc pvc = %+v", abc)
	}
}

// TestProbeStorageForbidden: all three lists fail; errs keep the assembly
// order storageclasses → persistentvolumes → persistentvolumeclaims.
func TestProbeStorageForbidden(t *testing.T) {
	client := fake.NewClientset()
	for _, res := range []string{"storageclasses", "persistentvolumes", "persistentvolumeclaims"} {
		res := res
		client.PrependReactor("list", res,
			func(action k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errForbidden()
			})
	}
	p := &prober{in: Input{Client: client, Namespace: "team-a"}}
	info, errs := p.probeStorage(context.Background())
	if len(errs) != 3 {
		t.Fatalf("errs = %+v, want 3", errs)
	}
	want := []string{"storageclasses", "persistentvolumes", "persistentvolumeclaims"}
	for i, r := range want {
		if errs[i].Section != "storage" || errs[i].Resource != r || errs[i].Code != "Forbidden" {
			t.Errorf("errs[%d] = %+v", i, errs[i])
		}
	}
	if !isZero(info) {
		t.Errorf("all lists denied should leave zero info: %+v", info)
	}
}

// TestProbeStoragePVCNamespaceMissing: NotFound from the namespaced PVC list
// is an empty fact, not an error.
func TestProbeStoragePVCNamespaceMissing(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("list", "persistentvolumeclaims",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewNotFound(
				schema.GroupResource{Resource: "persistentvolumeclaims"}, "")
		})
	p := &prober{in: Input{Client: client, Namespace: "ghost"}}
	info, errs := p.probeStorage(context.Background())
	if len(errs) != 0 {
		t.Fatalf("errs = %v, want none for NotFound", errs)
	}
	// gentype's fake List returns a non-nil empty list alongside the
	// NotFound, so the section records "probed and none", not "not probed".
	if info.PersistentVolumeClaims == nil || len(info.PersistentVolumeClaims) != 0 {
		t.Errorf("PVCs = %v, want empty slice", info.PersistentVolumeClaims)
	}
}
