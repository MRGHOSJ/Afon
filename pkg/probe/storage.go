package probe

import (
	"context"
	"sort"
	"sync"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type StorageClassInfo struct {
	Name                 string `json:"name"`
	Provisioner          string `json:"provisioner"`
	ReclaimPolicy        string `json:"reclaimPolicy,omitempty"`
	VolumeBindingMode    string `json:"volumeBindingMode,omitempty"`
	AllowVolumeExpansion bool   `json:"allowVolumeExpansion,omitempty"`
	IsDefault            bool   `json:"isDefault"`
}

type PVInfo struct {
	Name          string              `json:"name"`
	Status        string              `json:"status"`
	ReclaimPolicy string              `json:"reclaimPolicy,omitempty"`
	StorageClass  string              `json:"storageClass,omitempty"`
	Capacity      corev1.ResourceList `json:"capacity,omitempty"`
	AccessModes   []string            `json:"accessModes,omitempty"`
	ClaimRef      string              `json:"claimRef,omitempty"` // namespace/name
}

type PVCInfo struct {
	Name         string              `json:"name"`
	Phase        string              `json:"phase"`
	StorageClass string              `json:"storageClass,omitempty"`
	Requested    corev1.ResourceList `json:"requested,omitempty"`
	VolumeName   string              `json:"volumeName,omitempty"`
}

// StorageInfo: StorageClasses and PVs are cluster-scoped facts; PVCs are
// scoped to the target namespace (cluster-wide PVC lists are noisy).
type StorageInfo struct {
	StorageClasses         []StorageClassInfo `json:"storageClasses"`
	PersistentVolumes      []PVInfo           `json:"persistentVolumes"`
	PersistentVolumeClaims []PVCInfo          `json:"persistentVolumeClaims"`
}

func (p *prober) probeStorage(ctx context.Context) (StorageInfo, []ProbeError) {
	var info StorageInfo

	// StorageClasses, PVs and PVCs are independent lists; fetch concurrently
	// and assemble in the original order.
	var (
		wg      sync.WaitGroup
		scs     *storagev1.StorageClassList
		scsErr  *ProbeError
		pvs     *corev1.PersistentVolumeList
		pvsErr  *ProbeError
		pvcs    *corev1.PersistentVolumeClaimList
		pvcsErr *ProbeError
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		var err error
		scs, err = p.in.Client.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
		if err != nil {
			pe := asProbeError("storage", "storageclasses", err)
			scsErr = &pe
		}
	}()
	go func() {
		defer wg.Done()
		var err error
		pvs, err = p.in.Client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
		if err != nil {
			pe := asProbeError("storage", "persistentvolumes", err)
			pvsErr = &pe
		}
	}()
	go func() {
		defer wg.Done()
		var err error
		// Namespaced convention: NotFound → target namespace missing → empty fact.
		pvcs, err = p.in.Client.CoreV1().PersistentVolumeClaims(p.in.Namespace).List(ctx, metav1.ListOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			pe := asProbeError("storage", "persistentvolumeclaims", err)
			pvcsErr = &pe
		}
	}()
	wg.Wait()

	var errs []ProbeError
	if scsErr != nil {
		errs = append(errs, *scsErr)
	} else {
		info.StorageClasses = []StorageClassInfo{}
		for i := range scs.Items {
			sc := &scs.Items[i]
			info.StorageClasses = append(info.StorageClasses, StorageClassInfo{
				Name:                 sc.Name,
				Provisioner:          sc.Provisioner,
				ReclaimPolicy:        reclaimPolicy(sc),
				VolumeBindingMode:    bindingMode(sc),
				AllowVolumeExpansion: sc.AllowVolumeExpansion != nil && *sc.AllowVolumeExpansion,
				IsDefault:            sc.Annotations["storageclass.kubernetes.io/is-default-class"] == "true",
			})
		}
		sort.Slice(info.StorageClasses, func(i, j int) bool {
			return info.StorageClasses[i].Name < info.StorageClasses[j].Name
		})
	}

	if pvsErr != nil {
		errs = append(errs, *pvsErr)
	} else {
		info.PersistentVolumes = []PVInfo{}
		for i := range pvs.Items {
			pv := &pvs.Items[i]
			item := PVInfo{
				Name:          pv.Name,
				Status:        string(pv.Status.Phase),
				ReclaimPolicy: string(pv.Spec.PersistentVolumeReclaimPolicy),
				StorageClass:  pv.Spec.StorageClassName,
				Capacity:      pv.Spec.Capacity,
				AccessModes:   accessModes(pv),
			}
			if pv.Spec.ClaimRef != nil {
				item.ClaimRef = pv.Spec.ClaimRef.Namespace + "/" + pv.Spec.ClaimRef.Name
			}
			info.PersistentVolumes = append(info.PersistentVolumes, item)
		}
		sort.Slice(info.PersistentVolumes, func(i, j int) bool {
			return info.PersistentVolumes[i].Name < info.PersistentVolumes[j].Name
		})
	}

	if pvcsErr != nil {
		errs = append(errs, *pvcsErr)
	} else if pvcs != nil {
		info.PersistentVolumeClaims = []PVCInfo{}
		for i := range pvcs.Items {
			pvc := &pvcs.Items[i]
			item := PVCInfo{
				Name:       pvc.Name,
				Phase:      string(pvc.Status.Phase),
				Requested:  pvc.Spec.Resources.Requests,
				VolumeName: pvc.Spec.VolumeName,
			}
			if pvc.Spec.StorageClassName != nil {
				item.StorageClass = *pvc.Spec.StorageClassName
			}
			info.PersistentVolumeClaims = append(info.PersistentVolumeClaims, item)
		}
		sort.Slice(info.PersistentVolumeClaims, func(i, j int) bool {
			return info.PersistentVolumeClaims[i].Name < info.PersistentVolumeClaims[j].Name
		})
	}
	return info, errs
}

func reclaimPolicy(sc *storagev1.StorageClass) string {
	if sc.ReclaimPolicy != nil {
		return string(*sc.ReclaimPolicy)
	}
	return ""
}

func bindingMode(sc *storagev1.StorageClass) string {
	if sc.VolumeBindingMode != nil {
		return string(*sc.VolumeBindingMode)
	}
	return ""
}

func accessModes(pv *corev1.PersistentVolume) []string {
	var out []string
	for _, m := range pv.Spec.AccessModes {
		out = append(out, string(m))
	}
	return out
}
