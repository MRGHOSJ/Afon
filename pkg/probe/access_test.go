package probe

import (
	"context"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// TestProbeAccessSuccess: every SSAR completes, checks keep the accessChecks
// order, and the pods/log check is recorded with its combined name while the
// SSAR itself carries resource + subresource split.
func TestProbeAccessSuccess(t *testing.T) {
	client := fake.NewClientset()
	stubSSRR(client)
	var sawLogSplit, allNamespacesMatched = false, true
	client.PrependReactor("create", "selfsubjectaccessreviews",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			sar := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
			attrs := sar.Spec.ResourceAttributes
			if attrs.Namespace != "team-a" {
				allNamespacesMatched = false
			}
			if attrs.Resource == "pods" && attrs.Subresource == "log" && attrs.Verb == "get" {
				sawLogSplit = true
			}
			out := sar.DeepCopy()
			if attrs.Resource == "secrets" && attrs.Verb == "update" {
				out.Status.Allowed = false
				out.Status.Reason = "denied by policy"
			} else {
				out.Status.Allowed = true
			}
			return true, out, nil
		})
	p := &prober{in: Input{Client: client, Namespace: "team-a"}}
	info, errs := p.probeAccess(context.Background())

	if len(errs) != 0 {
		t.Fatalf("errs = %v, want none", errs)
	}
	if info.Namespace != "team-a" {
		t.Errorf("Namespace = %q", info.Namespace)
	}
	if info.Rules == nil || len(info.Rules) != 0 || info.RulesIncomplete {
		t.Errorf("Rules = %v incomplete=%v, want empty complete", info.Rules, info.RulesIncomplete)
	}
	if len(info.Checks) != len(accessChecks) {
		t.Fatalf("checks = %d, want %d", len(info.Checks), len(accessChecks))
	}
	if !allNamespacesMatched {
		t.Error("SSAR namespace not propagated")
	}
	if !sawLogSplit {
		t.Error("pods/log SSAR did not split subresource")
	}
	// Order matches accessChecks exactly: first, last, and a mid entry.
	first := info.Checks[0]
	if first.Resource != "namespaces" || first.Verb != "get" || !first.Allowed {
		t.Errorf("first check = %+v", first)
	}
	last := info.Checks[len(info.Checks)-1]
	if last.Group != "batch" || last.Resource != "jobs" || last.Verb != "create" || !last.Allowed {
		t.Errorf("last check = %+v", last)
	}
	denied := findCheck(info.Checks, "", "secrets", "update")
	if denied == nil {
		t.Fatal("secrets/update check missing")
	}
	if denied.Allowed || denied.Reason != "denied by policy" {
		t.Errorf("denied check = %+v", denied)
	}
	if log := findCheck(info.Checks, "", "pods/log", "get"); log == nil || !log.Allowed {
		t.Errorf("pods/log check = %+v", log)
	}
}

func TestProbeAccessRulesReviewForbidden(t *testing.T) {
	client := fake.NewClientset()
	stubSSAR(client)
	client.PrependReactor("create", "selfsubjectrulesreviews",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, errForbidden()
		})
	p := &prober{in: Input{Client: client, Namespace: "team-a"}}
	info, errs := p.probeAccess(context.Background())
	if len(errs) != 1 {
		t.Fatalf("errs = %+v, want 1", errs)
	}
	got := errs[0]
	if got.Section != "access" || got.Resource != "selfsubjectrulesreviews" ||
		got.Code != "Forbidden" {
		t.Errorf("ProbeError = %+v", got)
	}
	if info.Rules != nil {
		t.Errorf("Rules = %v, want nil without a review", info.Rules)
	}
	// The SSAR checks are independent and still all land.
	if len(info.Checks) != len(accessChecks) {
		t.Errorf("checks = %d, want %d", len(info.Checks), len(accessChecks))
	}
}

func TestProbeAccessIncompleteAndEvaluationError(t *testing.T) {
	client := fake.NewClientset()
	stubSSAR(client)
	specNamespace := ""
	client.PrependReactor("create", "selfsubjectrulesreviews",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			req := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectRulesReview)
			specNamespace = req.Spec.Namespace
			out := req.DeepCopy()
			out.Status = authorizationv1.SubjectRulesReviewStatus{
				ResourceRules: []authorizationv1.ResourceRule{
					{Verbs: []string{"*"}, APIGroups: []string{"*"}, Resources: []string{"*"}},
				},
				Incomplete:      true,
				EvaluationError: "authorizer note",
			}
			return true, out, nil
		})
	p := &prober{in: Input{Client: client, Namespace: "team-a"}}
	info, errs := p.probeAccess(context.Background())

	if specNamespace != "team-a" {
		t.Errorf("SSRR spec namespace = %q", specNamespace)
	}
	if !info.RulesIncomplete {
		t.Error("RulesIncomplete = false, want true")
	}
	if len(info.Rules) != 1 || info.Rules[0].Verbs[0] != "*" {
		t.Errorf("Rules = %+v", info.Rules)
	}
	if len(errs) != 2 {
		t.Fatalf("errs = %+v, want Incomplete + EvaluationError", errs)
	}
	if errs[0].Code != "Incomplete" || errs[0].Resource != "selfsubjectrulesreviews" {
		t.Errorf("errs[0] = %+v", errs[0])
	}
	if errs[1].Code != "EvaluationError" || errs[1].Reason != "authorizer note" {
		t.Errorf("errs[1] = %+v", errs[1])
	}
	if st := classify(info, errs); st.Status != StatusPartial {
		t.Errorf("classify = %+v, want partial", st)
	}
}

// TestProbeAccessCheckError: one failing SSAR drops only that check and
// records one structured error; the rest keep their order.
func TestProbeAccessCheckError(t *testing.T) {
	client := fake.NewClientset()
	stubSSRR(client)
	client.PrependReactor("create", "selfsubjectaccessreviews",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			sar := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
			attrs := sar.Spec.ResourceAttributes
			if attrs.Resource == "pods" && attrs.Verb == "delete" {
				return true, nil, errForbidden()
			}
			out := sar.DeepCopy()
			out.Status.Allowed = true
			return true, out, nil
		})
	p := &prober{in: Input{Client: client, Namespace: "team-a"}}
	info, errs := p.probeAccess(context.Background())

	if len(errs) != 1 {
		t.Fatalf("errs = %+v, want 1", errs)
	}
	got := errs[0]
	if got.Section != "access" || got.Resource != "selfsubjectaccessreviews/pods/delete" ||
		got.Code != "Forbidden" {
		t.Errorf("ProbeError = %+v", got)
	}
	if len(info.Checks) != len(accessChecks)-1 {
		t.Fatalf("checks = %d, want %d", len(info.Checks), len(accessChecks)-1)
	}
	if findCheck(info.Checks, "", "pods", "delete") != nil {
		t.Error("failed check still present")
	}
	if c := findCheck(info.Checks, "", "pods", "create"); c == nil || !c.Allowed {
		t.Errorf("sibling pods/create = %+v", c)
	}
}

// TestProbeAccessRulesSorted: rules come back scrambled from the authorizer
// and must be totally ordered by ruleKey.
func TestProbeAccessRulesSorted(t *testing.T) {
	client := fake.NewClientset()
	stubSSAR(client)
	client.PrependReactor("create", "selfsubjectrulesreviews",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			req := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectRulesReview)
			out := req.DeepCopy()
			out.Status.ResourceRules = []authorizationv1.ResourceRule{
				{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"create", "update"}},
				{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"}},
				{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"create", "delete"}},
			}
			out.Status.NonResourceRules = []authorizationv1.NonResourceRule{
				{NonResourceURLs: []string{"/metrics"}, Verbs: []string{"get"}},
			}
			return true, out, nil
		})
	p := &prober{in: Input{Client: client, Namespace: "team-a"}}
	info, errs := p.probeAccess(context.Background())
	if len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	if len(info.Rules) != 4 {
		t.Fatalf("rules = %d, want 4", len(info.Rules))
	}
	// Non-resource rule (empty groups) first; then core pods; then the two
	// apps rules ordered by verbs (delete before update).
	if len(info.Rules[0].NonResourceURLs) != 1 || info.Rules[0].NonResourceURLs[0] != "/metrics" {
		t.Errorf("rules[0] = %+v, want the non-resource rule", info.Rules[0])
	}
	if info.Rules[1].Resources[0] != "pods" {
		t.Errorf("rules[1] = %+v, want pods", info.Rules[1])
	}
	if info.Rules[2].Verbs[1] != "delete" || info.Rules[3].Verbs[1] != "update" {
		t.Errorf("apps rules out of order: %+v / %+v", info.Rules[2], info.Rules[3])
	}
}

func TestRuleKeyOrdering(t *testing.T) {
	a := PolicyRuleInfo{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list"}}
	b := PolicyRuleInfo{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "watch"}}
	c := PolicyRuleInfo{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"*"}}
	nonRes := PolicyRuleInfo{NonResourceURLs: []string{"/healthz"}, Verbs: []string{"get"}}
	if !(ruleKey(a) < ruleKey(b)) {
		t.Error("verbs must order a before b")
	}
	if !(ruleKey(a) < ruleKey(c)) {
		t.Error("api group must dominate")
	}
	if !(ruleKey(nonRes) < ruleKey(a)) {
		t.Error("non-resource rule must sort first")
	}
}

// The fake clientset's default create path cannot SSA-convert the review
// types (no schema in the OpenAPI set), so both review kinds need explicit
// stubs whenever a test does not install its own reactor for them.

// stubSSRR answers SelfSubjectRulesReview creates with an empty status.
func stubSSRR(client *fake.Clientset) {
	client.PrependReactor("create", "selfsubjectrulesreviews",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			req := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectRulesReview)
			return true, req.DeepCopy(), nil
		})
}

// stubSSAR allows every SelfSubjectAccessReview.
func stubSSAR(client *fake.Clientset) {
	client.PrependReactor("create", "selfsubjectaccessreviews",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			sar := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SelfSubjectAccessReview)
			out := sar.DeepCopy()
			out.Status.Allowed = true
			return true, out, nil
		})
}

func findCheck(checks []AccessCheck, group, resource, verb string) *AccessCheck {
	for i := range checks {
		c := &checks[i]
		if c.Group == group && c.Resource == resource && c.Verb == verb {
			return c
		}
	}
	return nil
}
