package probe

import (
	"context"
	"sort"
	"strings"

	"golang.org/x/sync/errgroup"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type PolicyRuleInfo struct {
	Verbs           []string `json:"verbs"`
	APIGroups       []string `json:"apiGroups,omitempty"`
	Resources       []string `json:"resources,omitempty"`
	ResourceNames   []string `json:"resourceNames,omitempty"`
	NonResourceURLs []string `json:"nonResourceURLs,omitempty"`
}

// AccessCheck is one explicit SSAR answer the reliable signal when the
// authorizer cannot produce a full rules review.
type AccessCheck struct {
	Group    string `json:"group,omitempty"`
	Resource string `json:"resource"`
	Verb     string `json:"verb"`
	Allowed  bool   `json:"allowed"`
	Reason   string `json:"reason,omitempty"`
}

type AccessInfo struct {
	Namespace       string           `json:"namespace"`
	Rules           []PolicyRuleInfo `json:"rules"`
	RulesIncomplete bool             `json:"rulesIncomplete"`
	Checks          []AccessCheck    `json:"checks"`
}

// accessChecks is the curated SSAR list: what the pipeline itself needs to
// do in the target namespace. SSA needs patch, prune needs delete, wait
// needs watch, in-cluster release records are Secrets, and debugging reads
// pods/log. A resource with a slash ("pods/log") is split into resource and
// subresource when the SSAR is built.
var accessChecks = []struct{ group, resource, verb string }{
	{"", "namespaces", "get"},
	{"", "events", "list"},
	{"", "pods", "list"},
	{"", "pods", "watch"},
	{"", "pods", "create"},
	{"", "pods", "delete"},
	{"", "pods/log", "get"},
	{"", "services", "create"},
	{"", "services", "patch"},
	{"", "services", "delete"},
	{"", "secrets", "get"},
	{"", "secrets", "create"},
	{"", "secrets", "update"},
	{"", "secrets", "patch"},
	{"", "secrets", "delete"},
	{"", "configmaps", "create"},
	{"", "configmaps", "patch"},
	{"", "configmaps", "delete"},
	{"", "persistentvolumeclaims", "create"},
	{"apps", "deployments", "create"},
	{"apps", "deployments", "update"},
	{"apps", "deployments", "get"},
	{"apps", "deployments", "patch"},
	{"apps", "deployments", "delete"},
	{"apps", "deployments", "watch"},
	{"networking.k8s.io", "ingresses", "create"},
	{"networking.k8s.io", "ingresses", "patch"},
	{"networking.k8s.io", "ingresses", "delete"},
	{"networking.k8s.io", "networkpolicies", "create"},
	{"autoscaling", "horizontalpodautoscalers", "create"},
	{"policy", "poddisruptionbudgets", "create"},
	{"batch", "jobs", "create"},
}

func (p *prober) probeAccess(ctx context.Context) (AccessInfo, []ProbeError) {
	info := AccessInfo{Namespace: p.in.Namespace}

	// The SSRR and the SSARs are independent round trips; run them
	// concurrently (bounded) instead of one-by-one. Each goroutine writes
	// only its own slot, so assembly below stays deterministic.
	var (
		g         errgroup.Group
		review    *authorizationv1.SelfSubjectRulesReview
		reviewErr error
	)
	g.SetLimit(8)
	g.Go(func() error {
		review, reviewErr = p.in.Client.AuthorizationV1().SelfSubjectRulesReviews().Create(
			ctx,
			&authorizationv1.SelfSubjectRulesReview{
				Spec: authorizationv1.SelfSubjectRulesReviewSpec{Namespace: p.in.Namespace},
			},
			metav1.CreateOptions{},
		)
		return nil
	})

	checks := make([]*AccessCheck, len(accessChecks))
	checkErrs := make([]*ProbeError, len(accessChecks))
	for i, c := range accessChecks {
		g.Go(func() error {
			res, sub := c.resource, ""
			if j := strings.IndexByte(c.resource, '/'); j >= 0 {
				res, sub = c.resource[:j], c.resource[j+1:]
			}
			check := AccessCheck{Group: c.group, Resource: c.resource, Verb: c.verb}
			sar, err := p.in.Client.AuthorizationV1().SelfSubjectAccessReviews().Create(
				ctx,
				&authorizationv1.SelfSubjectAccessReview{
					Spec: authorizationv1.SelfSubjectAccessReviewSpec{
						ResourceAttributes: &authorizationv1.ResourceAttributes{
							Namespace:   p.in.Namespace,
							Group:       c.group,
							Resource:    res,
							Subresource: sub,
							Verb:        c.verb,
						},
					},
				},
				metav1.CreateOptions{},
			)
			if err != nil {
				pe := asProbeError("access",
					"selfsubjectaccessreviews/"+c.resource+"/"+c.verb, err)
				checkErrs[i] = &pe
				return nil
			}
			check.Allowed = sar.Status.Allowed
			check.Reason = sar.Status.Reason
			checks[i] = &check
			return nil
		})
	}
	g.Wait()

	// Assemble in logical order (SSRR first, then accessChecks order), never
	// completion order, so profiles stay deterministic.
	var errs []ProbeError
	if reviewErr != nil {
		errs = append(errs, asProbeError("access", "selfsubjectrulesreviews", reviewErr))
	} else {
		info.Rules = []PolicyRuleInfo{}
		for _, r := range review.Status.ResourceRules {
			info.Rules = append(info.Rules, PolicyRuleInfo{
				Verbs:         r.Verbs,
				APIGroups:     r.APIGroups,
				Resources:     r.Resources,
				ResourceNames: r.ResourceNames,
			})
		}
		for _, r := range review.Status.NonResourceRules {
			info.Rules = append(info.Rules, PolicyRuleInfo{
				Verbs:           r.Verbs,
				NonResourceURLs: r.NonResourceURLs,
			})
		}
		// The API server orders rules per run; sort so saved profiles and
		// fixtures compare clean.
		sort.Slice(info.Rules, func(i, j int) bool {
			return ruleKey(info.Rules[i]) < ruleKey(info.Rules[j])
		})
		if review.Status.Incomplete {
			// Some authorizers do not support SSRR; the SSAR checks below are
			// the reliable signal. Surfaced as an error so classify() marks
			// the section partial while keeping the rules we did get.
			info.RulesIncomplete = true
			errs = append(errs, ProbeError{
				Section: "access", Resource: "selfsubjectrulesreviews",
				Reason: "SelfSubjectRulesReview incomplete (authorizer may not support it)",
				Code:   "Incomplete",
			})
		}
		if msg := review.Status.EvaluationError; msg != "" {
			errs = append(errs, ProbeError{
				Section: "access", Resource: "selfsubjectrulesreviews",
				Reason: msg, Code: "EvaluationError",
			})
		}
	}

	info.Checks = []AccessCheck{}
	for i := range accessChecks {
		if checkErrs[i] != nil {
			errs = append(errs, *checkErrs[i])
			continue
		}
		info.Checks = append(info.Checks, *checks[i])
	}
	return info, errs
}

// ruleKey gives one SSRR rule a total order across every field, so sorting
// on it is deterministic even for rules that differ only in verbs.
func ruleKey(r PolicyRuleInfo) string {
	return strings.Join(r.APIGroups, ",") + "\x00" +
		strings.Join(r.Resources, ",") + "\x00" +
		strings.Join(r.ResourceNames, ",") + "\x00" +
		strings.Join(r.Verbs, ",") + "\x00" +
		strings.Join(r.NonResourceURLs, ",")
}
