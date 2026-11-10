package probe

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/version"
	dfake "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestParseMinorString(t *testing.T) {
	okCases := []struct {
		in   string
		want int
	}{
		{in: "35", want: 35},
		{in: "35+", want: 35},
		{in: "v1.35.5+k3s1", want: 35},
		{in: "1.35", want: 35},
		{in: "v0.37.2", want: 37},
		{in: "0.37.2", want: 37},
		{in: "1.30.0-rc.1", want: 30},
	}
	for _, tc := range okCases {
		got, ok := parseMinorString(tc.in)
		if !ok || got != tc.want {
			t.Errorf("parseMinorString(%q) = (%d, %v), want (%d, true)", tc.in, got, ok, tc.want)
		}
	}
	failCases := []string{"", "abc", "x.y.z", "v", "35+garbage"}
	for _, in := range failCases {
		if got, ok := parseMinorString(in); ok {
			t.Errorf("parseMinorString(%q) = (%d, true), want failure", in, got)
		}
	}
}

func TestParseMinorFallback(t *testing.T) {
	if got, ok := parseMinor("", "v1.34.0"); !ok || got != 34 {
		t.Errorf("parseMinor fallback = (%d, %v), want (34, true)", got, ok)
	}
	if got, ok := parseMinor("34+", "junk"); !ok || got != 34 {
		t.Errorf("parseMinor minorField = (%d, %v), want (34, true)", got, ok)
	}
	if _, ok := parseMinor("junk", "junk2"); ok {
		t.Error("parseMinor(junk, junk2) succeeded, want failure")
	}
}

// TestClientGoVersion just has to report something parseable the skew math
// depends on it; the exact value tracks the linked client-go version.
func TestClientGoVersion(t *testing.T) {
	v := clientGoVersion()
	if v == "" {
		t.Fatal("clientGoVersion() = empty")
	}
	if _, ok := parseMinorVersion(v); !ok {
		t.Errorf("clientGoVersion() = %q, not minor-parseable", v)
	}
}

// TestProbeVersionSkew derives the client's minor version at runtime so the
// test does not hardcode the linked client-go release.
func TestProbeVersionSkew(t *testing.T) {
	clientMinor, ok := parseMinorVersion(clientGoVersion())
	if !ok {
		t.Fatalf("client version %q not parseable", clientGoVersion())
	}
	cases := []struct {
		name       string
		minorField string
		gitVersion string
		wantDelta  int
		wantSkewOK bool
	}{
		{name: "same minor", minorField: strconv.Itoa(clientMinor), gitVersion: "v1.0.0", wantDelta: 0, wantSkewOK: true},
		{name: "one ahead", minorField: strconv.Itoa(clientMinor + 1), gitVersion: "v1.0.0", wantDelta: 1, wantSkewOK: true},
		{name: "two behind", minorField: strconv.Itoa(clientMinor - 2), gitVersion: "v1.0.0", wantDelta: 2, wantSkewOK: false},
		{name: "minor from gitVersion only", minorField: "", gitVersion: "v1." + strconv.Itoa(clientMinor+1) + ".0", wantDelta: 1, wantSkewOK: true},
		{name: "unparseable server minor", minorField: "weird", gitVersion: "weird", wantDelta: 0, wantSkewOK: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewClientset()
			client.Discovery().(*dfake.FakeDiscovery).FakedServerVersion = &version.Info{
				GitVersion: tc.gitVersion,
				Major:      "1",
				Minor:      tc.minorField,
				GitCommit:  "abc123",
			}
			p := &prober{in: Input{Client: client}}
			v, errs := p.probeVersion(context.Background())
			if len(errs) != 0 {
				t.Fatalf("errs = %v, want none", errs)
			}
			if v.Server.GitCommit != "abc123" || v.Server.Major != "1" {
				t.Errorf("server fields not copied: %+v", v.Server)
			}
			if v.MinorDelta != tc.wantDelta {
				t.Errorf("MinorDelta = %d, want %d", v.MinorDelta, tc.wantDelta)
			}
			if v.WithinSkewWindow != tc.wantSkewOK {
				t.Errorf("WithinSkewWindow = %v, want %v", v.WithinSkewWindow, tc.wantSkewOK)
			}
			if v.Client != clientGoVersion() {
				t.Errorf("Client = %q, want %q", v.Client, clientGoVersion())
			}
		})
	}
}

// TestProbeVersionServerError covers the hard-failure seed: the version probe
// records the raw error for later classification and reports a ProbeError.
func TestProbeVersionServerError(t *testing.T) {
	boom := errors.New("kaboom")
	client := fake.NewClientset()
	client.PrependReactor("get", "version",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, boom
		})
	p := &prober{in: Input{Client: client}}
	v, errs := p.probeVersion(context.Background())
	if p.versionErr == nil {
		t.Fatal("versionErr not recorded")
	}
	if !errors.Is(p.versionErr, boom) {
		t.Errorf("versionErr = %v, want wrapped boom", p.versionErr)
	}
	if !isZero(v) {
		t.Errorf("data = %+v, want zero value", v)
	}
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want 1", errs)
	}
	got := errs[0]
	if got.Section != "version" || got.Resource != "serverversion" || got.Reason != "kaboom" {
		t.Errorf("ProbeError = %+v", got)
	}
	if st := classify(v, errs); st.Status != StatusPartial {
		t.Errorf("classify = %+v, want partial", st)
	}
}

func TestIsDigits(t *testing.T) {
	if !isDigits("123") || !isDigits("0") {
		t.Error("isDigits digit strings = false")
	}
	if isDigits("") || isDigits("1a") || isDigits("1.2") {
		t.Error("isDigits non-digit strings = true")
	}
}
