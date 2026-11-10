package probe

import (
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestClassify(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("nope"))
	notFound := apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "x")
	noMatch := &meta.NoResourceMatchError{PartialResource: schema.GroupVersionResource{Resource: "widgets"}}
	generic := errors.New("boom")
	withData := VersionInfo{Server: ServerVersionInfo{GitVersion: "v1.30.0"}}

	cases := []struct {
		name   string
		data   any
		errs   []ProbeError
		status string
		reason string
	}{
		{name: "no errors", data: VersionInfo{}, errs: nil, status: StatusOK},
		{name: "no errors with data", data: withData, errs: nil, status: StatusOK},
		{
			name: "data plus errors is partial",
			data: withData,
			errs: []ProbeError{{Reason: "first"}, {Reason: "second"}},
			// The first error's reason becomes the section reason.
			status: StatusPartial, reason: "first",
		},
		{
			name: "all forbidden", data: VersionInfo{},
			errs: []ProbeError{
				asProbeError("s", "r", forbidden),
				asProbeError("s", "other", forbidden),
			},
			status: StatusDenied, reason: forbidden.Error(),
		},
		{
			name: "all not found", data: VersionInfo{},
			errs: []ProbeError{
				asProbeError("s", "r", notFound),
				asProbeError("s", "r", notFound),
			},
			status: StatusUnavailable, reason: notFound.Error(),
		},
		{
			name: "all no match", data: VersionInfo{},
			errs: []ProbeError{
				asProbeError("s", "r", noMatch),
				asProbeError("s", "r", noMatch),
			},
			status: StatusUnavailable, reason: noMatch.Error(),
		},
		{
			name: "mixed codes with no data", data: VersionInfo{},
			errs: []ProbeError{
				asProbeError("s", "r", forbidden),
				asProbeError("s", "r", notFound),
			},
			status: StatusPartial, reason: forbidden.Error(),
		},
		{
			name: "generic error with no data", data: VersionInfo{},
			errs:   []ProbeError{asProbeError("s", "r", generic)},
			status: StatusPartial, reason: generic.Error(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(tc.data, tc.errs)
			if got.Status != tc.status {
				t.Errorf("Status = %q, want %q", got.Status, tc.status)
			}
			if got.Reason != tc.reason {
				t.Errorf("Reason = %q, want %q", got.Reason, tc.reason)
			}
		})
	}
}

func TestSortErrors(t *testing.T) {
	errs := []ProbeError{
		{Section: "version", Resource: "b", Reason: "z"},
		{Section: "access", Resource: "z", Reason: "a"},
		{Section: "access", Resource: "a", Reason: "z"},
		{Section: "access", Resource: "a", Reason: "a"},
		{Section: "access", Resource: "a", Reason: "a", Code: "Ignored"},
	}
	sortErrors(errs)
	want := []ProbeError{
		{Section: "access", Resource: "a", Reason: "a"},
		{Section: "access", Resource: "a", Reason: "a", Code: "Ignored"},
		{Section: "access", Resource: "a", Reason: "z"},
		{Section: "access", Resource: "z", Reason: "a"},
		{Section: "version", Resource: "b", Reason: "z"},
	}
	if len(errs) != len(want) {
		t.Fatalf("len = %d, want %d", len(errs), len(want))
	}
	for i := range want {
		if errs[i] != want[i] {
			t.Errorf("errs[%d] = %+v, want %+v", i, errs[i], want[i])
		}
	}
}

func TestAsProbeError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code string
	}{
		{name: "forbidden", err: apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "", errors.New("x")), code: "Forbidden"},
		{name: "not found", err: apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "x"), code: "NotFound"},
		{name: "no match", err: &meta.NoResourceMatchError{PartialResource: schema.GroupVersionResource{Resource: "w"}}, code: "NoMatch"},
		// ReasonForError returns metav1.StatusReasonUnknown whose value is "".
		{name: "generic", err: errors.New("boom"), code: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pe := asProbeError("section", "resource", tc.err)
			if pe.Section != "section" || pe.Resource != "resource" {
				t.Errorf("identity fields wrong: %+v", pe)
			}
			if pe.Code != tc.code {
				t.Errorf("Code = %q, want %q", pe.Code, tc.code)
			}
			if pe.Reason != tc.err.Error() {
				t.Errorf("Reason = %q, want %q", pe.Reason, tc.err.Error())
			}
		})
	}
}

func TestProbeErrorError(t *testing.T) {
	withResource := ProbeError{Section: "nodes", Resource: "pods", Reason: "denied"}
	if got, want := withResource.Error(), "nodes: pods: denied"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	without := ProbeError{Section: "version", Reason: "refused"}
	if got, want := without.Error(), "version: refused"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestIsZero(t *testing.T) {
	var nilPtr *VersionInfo
	cases := []struct {
		name string
		v    any
		want bool
	}{
		{name: "nil any", v: nil, want: true},
		{name: "zero struct", v: VersionInfo{}, want: true},
		{name: "populated", v: VersionInfo{MinorDelta: 1}, want: false},
		{name: "nil pointer", v: nilPtr, want: true},
		{name: "pointer to zero", v: &VersionInfo{}, want: false},
		{name: "zero string", v: "", want: true},
		{name: "nil slice", v: []string(nil), want: true},
		{name: "empty non-nil slice", v: []string{}, want: false},
		{name: "non-empty slice", v: []string{"x"}, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isZero(tc.v); got != tc.want {
				t.Errorf("isZero(%v) = %v, want %v", tc.v, got, tc.want)
			}
		})
	}
}
