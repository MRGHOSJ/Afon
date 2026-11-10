package probe

import (
	"context"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ResourceQuotaInfo struct {
	Name string              `json:"name"`
	Hard corev1.ResourceList `json:"hard,omitempty"`
	Used corev1.ResourceList `json:"used,omitempty"`
}

type LimitRangeItemInfo struct {
	Type           string              `json:"type"`
	Max            corev1.ResourceList `json:"max,omitempty"`
	Min            corev1.ResourceList `json:"min,omitempty"`
	Default        corev1.ResourceList `json:"default,omitempty"`
	DefaultRequest corev1.ResourceList `json:"defaultRequest,omitempty"`
}

type LimitRangeInfo struct {
	Name  string               `json:"name"`
	Items []LimitRangeItemInfo `json:"items"`
}

// NamespaceInfo describes the target namespace. Labels carry the Pod Security
// Admission decisions (pod-security.kubernetes.io/*) the engine must honour.
type NamespaceInfo struct {
	Exists         bool                `json:"exists"`
	Name           string              `json:"name"`
	UID            string              `json:"uid,omitempty"`
	Phase          string              `json:"phase,omitempty"`
	CreatedAt      *time.Time          `json:"createdAt,omitempty"`
	Labels         map[string]string   `json:"labels,omitempty"`
	Annotations    map[string]string   `json:"annotations,omitempty"`
	ResourceQuotas []ResourceQuotaInfo `json:"resourceQuotas,omitempty"`
	LimitRanges    []LimitRangeInfo    `json:"limitRanges,omitempty"`
}

func (p *prober) probeNamespace(ctx context.Context) (NamespaceInfo, []ProbeError) {
	// The namespace get and the two namespaced lists are independent round
	// trips; run them concurrently. A missing namespace is decided by the get,
	// so the other results are discarded in that case (they would only carry
	// the same NotFound).
	var (
		wg        sync.WaitGroup
		ns        *corev1.Namespace
		nsErr     error
		quotas    *corev1.ResourceQuotaList
		quotasErr *ProbeError
		ranges    *corev1.LimitRangeList
		rangesErr *ProbeError
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		var err error
		ns, err = p.in.Client.CoreV1().Namespaces().Get(ctx, p.in.Namespace, metav1.GetOptions{})
		nsErr = err
	}()
	go func() {
		defer wg.Done()
		var err error
		quotas, err = p.in.Client.CoreV1().ResourceQuotas(p.in.Namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			pe := asProbeError("namespace", "resourcequotas", err)
			quotasErr = &pe
		}
	}()
	go func() {
		defer wg.Done()
		var err error
		ranges, err = p.in.Client.CoreV1().LimitRanges(p.in.Namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			pe := asProbeError("namespace", "limitranges", err)
			rangesErr = &pe
		}
	}()
	wg.Wait()

	if nsErr != nil {
		if apierrors.IsNotFound(nsErr) {
			// A missing target namespace is a fact, not a failure.
			return NamespaceInfo{Name: p.in.Namespace}, nil
		}
		return NamespaceInfo{}, []ProbeError{asProbeError("namespace", "namespaces/"+p.in.Namespace, nsErr)}
	}

	created := ns.CreationTimestamp.Time.UTC()
	info := NamespaceInfo{
		Exists:      true,
		Name:        ns.Name,
		UID:         string(ns.UID),
		Phase:       string(ns.Status.Phase),
		CreatedAt:   &created,
		Labels:      allowMap(ns.Labels),
		Annotations: allowMap(ns.Annotations),
	}

	var errs []ProbeError
	if quotasErr != nil {
		errs = append(errs, *quotasErr)
	} else {
		for i := range quotas.Items {
			q := &quotas.Items[i]
			info.ResourceQuotas = append(info.ResourceQuotas, ResourceQuotaInfo{
				Name: q.Name, Hard: q.Status.Hard, Used: q.Status.Used,
			})
		}
	}

	if rangesErr != nil {
		errs = append(errs, *rangesErr)
	} else {
		for i := range ranges.Items {
			lr := &ranges.Items[i]
			li := LimitRangeInfo{Name: lr.Name}
			for _, item := range lr.Spec.Limits {
				li.Items = append(li.Items, LimitRangeItemInfo{
					Type:           string(item.Type),
					Max:            item.Max,
					Min:            item.Min,
					Default:        item.Default,
					DefaultRequest: item.DefaultRequest,
				})
			}
			info.LimitRanges = append(info.LimitRanges, li)
		}
	}
	return info, errs
}
