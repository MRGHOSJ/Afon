package probe

import (
	"fmt"
	"reflect"
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
)

const (
	StatusOK          = "ok"
	StatusPartial     = "partial"
	StatusDenied      = "denied"
	StatusUnavailable = "unavailable"
)

// SectionStatus reports how one probe section fared. ok: all calls succeeded;
// partial: some calls succeeded and some failed (data kept); denied: every call
// was forbidden; unavailable: every call failed with not-found/no-match (the
// capability or namespace does not exist).
type SectionStatus struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
	// DurationMs is how long this section took, so a slow probe is visible
	// in the profile instead of hidden behind the parallel wait.
	DurationMs int64 `json:"durationMs"`
}

// ProbeError is a structured per-section failure. Code carries the k8s
// StatusReason ("Forbidden", "NotFound", ...) or a synthetic marker
// ("NoMatch", "PartialDiscovery", "Incomplete", "Unsupported", "Error").
type ProbeError struct {
	Section  string `json:"section"`
	Resource string `json:"resource,omitempty"`
	Reason   string `json:"reason"`
	Code     string `json:"code,omitempty"`
}

func (e ProbeError) Error() string {
	if e.Resource != "" {
		return fmt.Sprintf("%s: %s: %s", e.Section, e.Resource, e.Reason)
	}
	return e.Section + ": " + e.Reason
}

// asProbeError converts a client error into a structured ProbeError.
func asProbeError(section, resource string, err error) ProbeError {
	pe := ProbeError{Section: section, Resource: resource, Reason: err.Error()}
	switch {
	case meta.IsNoMatchError(err):
		pe.Code = "NoMatch"
	default:
		pe.Code = string(apierrors.ReasonForError(err))
	}
	return pe
}

// classify derives a section status from the probe's data and errors.
// A probe that returned usable data along with errors is always partial;
// otherwise the error codes decide.
func classify(data any, errs []ProbeError) SectionStatus {
	if len(errs) == 0 {
		return SectionStatus{Status: StatusOK}
	}
	reason := errs[0].Reason
	if !isZero(data) {
		return SectionStatus{Status: StatusPartial, Reason: reason}
	}
	allDenied, allGone := true, true
	for _, e := range errs {
		if e.Code != "Forbidden" {
			allDenied = false
		}
		if e.Code != "NotFound" && e.Code != "NoMatch" {
			allGone = false
		}
	}
	switch {
	case allDenied:
		return SectionStatus{Status: StatusDenied, Reason: reason}
	case allGone:
		return SectionStatus{Status: StatusUnavailable, Reason: reason}
	default:
		return SectionStatus{Status: StatusPartial, Reason: reason}
	}
}

func isZero(v any) bool {
	if v == nil {
		return true
	}
	return reflect.ValueOf(v).IsZero()
}

// sortErrors gives the composer's error list a deterministic order; probes run
// in parallel so append order is racy.
func sortErrors(errs []ProbeError) {
	sort.Slice(errs, func(i, j int) bool {
		if errs[i].Section != errs[j].Section {
			return errs[i].Section < errs[j].Section
		}
		if errs[i].Resource != errs[j].Resource {
			return errs[i].Resource < errs[j].Resource
		}
		return errs[i].Reason < errs[j].Reason
	})
}
