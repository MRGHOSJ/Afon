package k8s

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"

	"github.com/MRGHOSJ/afon/pkg/driver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// volatile is the strip list the Stage 2a spike ratified against live
// API responses: server bookkeeping and status never participate in
// classification. Keys are stripped at any depth. The last-applied
// annotation is client-side-apply baggage (SSA never writes it) but is
// stripped anyway so an object imported from kubectl cannot
// phantom-diff forever.
var volatile = map[string]bool{
	"resourceVersion":   true,
	"managedFields":     true,
	"generation":        true,
	"creationTimestamp": true,
	"selfLink":          true,
	"uid":               true,
	"status":            true,
}

const lastAppliedAnno = "kubectl.kubernetes.io/last-applied-configuration"

// stripVolatile removes volatile keys at any depth, including the
// last-applied annotation from within annotations.
func stripVolatile(v any) any {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if volatile[k] {
				continue
			}
			if k == "annotations" {
				if m, ok := val.(map[string]any); ok {
					cp := make(map[string]any, len(m))
					for ak, av := range m {
						if ak == lastAppliedAnno {
							continue
						}
						cp[ak] = av
					}
					val = cp
				}
			}
			out[k] = stripVolatile(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = stripVolatile(e)
		}
		return out
	default:
		return v
	}
}

// canonical is the single comparison form for classification: strip
// volatile keys, then the same normalization hashing uses (engine
// rule 7 — map order and 1 vs 1.0 can never produce a phantom diff),
// then sorted-key JSON. Equal states produce byte-identical output.
func canonical(m map[string]any) ([]byte, error) {
	norm, err := driver.Normalize(stripVolatile(m))
	if err != nil {
		return nil, fmt.Errorf("normalizing object: %w", err)
	}
	return json.Marshal(norm)
}

// sameState is plan's classification oracle: the dry-run apply result
// against the live GET. True means applying would change nothing.
func sameState(a, b map[string]any) bool {
	ca, err := canonical(a)
	if err != nil {
		return false
	}
	cb, err := canonical(b)
	if err != nil {
		return false
	}
	return bytes.Equal(ca, cb)
}

// ssaConflictRe matches the API server's field-manager dispute
// message, e.g. `conflict with "spike-other": .metadata.labels.team`.
// The Stage 2a spike confirmed dry-run and real apply produce the
// identical string, so plan and apply surface the same conflicts.
// Optimistic-concurrency 409s never match, so they stay plain errors:
// driver.ErrConflict is documented as field disputes only.
var ssaConflictRe = regexp.MustCompile(`conflict with "([^"]+)"`)

// asConflict converts an SSA 409 into *driver.ErrConflict, listing the
// managers found in the status message (deduplicated, in order). It
// returns nil when the error is not a field-manager dispute — the
// caller then wraps it as an ordinary failure.
func asConflict(o driver.Object, err error) error {
	if err == nil || !apierrors.IsConflict(err) {
		return nil
	}
	found := ssaConflictRe.FindAllStringSubmatch(err.Error(), -1)
	if len(found) == 0 {
		return nil
	}
	managers := make([]string, 0, len(found))
	seen := map[string]bool{}
	for _, m := range found {
		if !seen[m[1]] {
			seen[m[1]] = true
			managers = append(managers, m[1])
		}
	}
	return &driver.ErrConflict{Object: o, Managers: managers}
}

// patchErr prefers the typed conflict (which carries the --force hint)
// and gives everything else object identity context.
func patchErr(o driver.Object, phase string, err error) error {
	if c := asConflict(o, err); c != nil {
		return c
	}
	return fmt.Errorf("%s %s/%s: %w", phase, o.Kind, o.Name, err)
}

// applyTier orders mutations within one call: dependencies first for
// creates and updates (a ConfigMap before the Deployment that mounts
// it), reversed for deletes so dependents go first. Unknown kinds —
// CRDs, mostly — sort at defaultTier: between workloads and jobs,
// alphabetical within the tier. The table is deliberately small; it
// orders a single flow's objects, it is not a cluster installer.
var applyTier = map[string]int{
	"ConfigMap": 10, "Secret": 10, "ServiceAccount": 10, "PersistentVolumeClaim": 10,
	"LimitRange": 10, "ResourceQuota": 10,
	"Role": 20, "RoleBinding": 20, "NetworkPolicy": 20,
	"Service":    30,
	"Deployment": 40, "StatefulSet": 40, "DaemonSet": 40, "ReplicaSet": 40,
	"Job": 50, "CronJob": 50,
	"Ingress": 60, "HorizontalPodAutoscaler": 60, "PodDisruptionBudget": 60,
}

const defaultTier = 45

func tier(kind string) int {
	if t, ok := applyTier[kind]; ok {
		return t
	}
	return defaultTier
}

// lessDiff is the total order for one direction of movement: tier,
// then kind, then name, then full key so equal kinds never tie.
func lessDiff(a, b driver.Diff) bool {
	if ta, tb := tier(a.Object.Kind), tier(b.Object.Kind); ta != tb {
		return ta < tb
	}
	if a.Object.Kind != b.Object.Kind {
		return a.Object.Kind < b.Object.Kind
	}
	if a.Object.Name != b.Object.Name {
		return a.Object.Name < b.Object.Name
	}
	return a.Object.Key() < b.Object.Key()
}

// sortDiffs puts creates/updates/none in dependency order and deletes
// last, each group sorted by lessDiff; the delete group is then
// reversed so the highest tier is removed first — reverse priority
// among deletes. Plan returns diffs in this order and Apply executes
// in it, so what the user sees is what runs.
func sortDiffs(diffs []driver.Diff) {
	up := make([]driver.Diff, 0, len(diffs))
	del := make([]driver.Diff, 0, len(diffs))
	for _, df := range diffs {
		if df.Op == driver.OpDelete {
			del = append(del, df)
		} else {
			up = append(up, df)
		}
	}
	sort.SliceStable(up, func(i, j int) bool { return lessDiff(up[i], up[j]) })
	sort.SliceStable(del, func(i, j int) bool { return lessDiff(del[i], del[j]) })
	for i, j := 0, len(del)-1; i < j; i, j = i+1, j-1 {
		del[i], del[j] = del[j], del[i]
	}
	out := append(up, del...)
	copy(diffs, out)
}
