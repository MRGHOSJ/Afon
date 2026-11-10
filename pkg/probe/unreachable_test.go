package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
)

func TestClassifyUnreachable(t *testing.T) {
	windowsRefused := &url.Error{
		Op:  "Get",
		URL: "https://127.0.0.1:53502/version",
		Err: &os.SyscallError{Syscall: "connectex", Err: syscall.ECONNREFUSED},
	}
	linuxRefused := &url.Error{
		Op:  "Get",
		URL: "https://127.0.0.1:6443/version",
		Err: &net.OpError{
			Op:  "dial",
			Net: "tcp",
			Err: os.NewSyscallError("connect", syscall.ECONNREFUSED),
		},
	}
	flatRefused := errors.New("dial tcp 127.0.0.1:53502: connectex: actively refused")
	dnsErr := &url.Error{
		Op:  "Get",
		URL: "https://nope.local/version",
		Err: &net.OpError{
			Op:  "dial",
			Net: "tcp",
			Err: &net.DNSError{Err: "no such host", Name: "nope.local", IsNotFound: true},
		},
	}
	deadline := &url.Error{
		Op:  "Get",
		URL: "https://10.0.0.9:6443/version",
		Err: context.DeadlineExceeded,
	}
	tlsErr := &url.Error{
		Op:  "Get",
		URL: "https://10.0.0.9:6443/version",
		Err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}},
	}
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "version"}, "", errors.New("nope"))
	unauthorized := apierrors.NewUnauthorized("bad credentials")
	odd := errors.New("something odd")

	cases := []struct {
		name     string
		err      error
		verb     string
		cause    string   // exact match when set
		contains []string // substrings the cause must carry (cause left "" instead)
		hint     string
	}{
		{name: "windows connection refused", err: windowsRefused,
			verb: "cannot reach", cause: "connection refused", hint: "is the cluster running?"},
		{name: "linux connection refused", err: linuxRefused,
			verb: "cannot reach", cause: "connection refused", hint: "is the cluster running?"},
		{name: "flattened refused wording", err: flatRefused,
			verb: "cannot reach", cause: "connection refused", hint: "is the cluster running?"},
		{name: "dns failure", err: dnsErr,
			verb: "cannot reach", cause: "cannot resolve host",
			hint: "check the server address in your kubeconfig"},
		{name: "timeout", err: deadline,
			verb: "cannot reach", cause: "timed out after 15s",
			hint: "check the network path (firewall/VPN)"},
		{name: "tls verification", err: tlsErr,
			verb: "cannot reach", contains: []string{"x509:"},
			hint: "the certificate may have been reissued — refresh your kubeconfig"},
		{name: "forbidden", err: forbidden,
			verb: "not authorized for", contains: []string{"forbidden"},
			hint: "check your kubeconfig credentials"},
		{name: "unauthorized", err: unauthorized,
			verb: "not authorized for", cause: "bad credentials",
			hint: "check your kubeconfig credentials"},
		{name: "unclassified keeps raw cause", err: odd,
			verb: "cannot reach", cause: "something odd", hint: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verb, cause, hint := classifyUnreachable(tc.err)
			if verb != tc.verb {
				t.Errorf("verb = %q, want %q", verb, tc.verb)
			}
			if tc.cause != "" {
				if cause != tc.cause {
					t.Errorf("cause = %q, want %q", cause, tc.cause)
				}
			}
			for _, want := range tc.contains {
				if !strings.Contains(cause, want) {
					t.Errorf("cause = %q, want it to contain %q", cause, want)
				}
			}
			if hint != tc.hint {
				t.Errorf("hint = %q, want %q", hint, tc.hint)
			}
		})
	}
}

func TestUnreachableErrorMessage(t *testing.T) {
	cases := []struct {
		name string
		e    *UnreachableError
		want string
	}{
		{
			name: "context and server",
			e: &UnreachableError{
				Context: "k3d-dev", Server: "https://127.0.0.1:53502",
				Cause: "connection refused", Hint: "is the cluster running?",
				verb: "cannot reach",
			},
			want: `cannot reach cluster "k3d-dev" (https://127.0.0.1:53502): connection refused — is the cluster running?`,
		},
		{
			name: "server only",
			e:    &UnreachableError{Server: "https://10.0.0.9:6443", Cause: "boom", verb: "cannot reach"},
			want: "cannot reach cluster (https://10.0.0.9:6443): boom",
		},
		{
			name: "context only",
			e:    &UnreachableError{Context: "prod", Cause: "boom", verb: "cannot reach"},
			want: `cannot reach cluster "prod": boom`,
		},
		{
			name: "no identity",
			e:    &UnreachableError{Cause: "boom", verb: "cannot reach"},
			want: "cannot reach cluster: boom",
		},
		{
			name: "auth verb",
			e: &UnreachableError{
				Context: "prod", Server: "https://10.0.0.9:6443",
				Cause: "version is forbidden: nope", Hint: "check your kubeconfig credentials",
				verb: "not authorized for",
			},
			want: `not authorized for cluster "prod" (https://10.0.0.9:6443): version is forbidden: nope — check your kubeconfig credentials`,
		},
		{
			name: "zero-value verb defaults",
			e:    &UnreachableError{Cause: "boom"},
			want: "cannot reach cluster: boom",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.e.Error(); got != tc.want {
				t.Errorf("Error() =\n  %q\nwant\n  %q", got, tc.want)
			}
		})
	}
}

func TestUnreachableErrorFromInput(t *testing.T) {
	e := unreachableError(Input{
		KubeContext: "k3d-dev",
		RESTConfig:  &rest.Config{Host: "https://127.0.0.1:53502"},
	}, errors.New("connection refused"))
	want := `cannot reach cluster "k3d-dev" (https://127.0.0.1:53502): connection refused — is the cluster running?`
	if got := e.Error(); got != want {
		t.Errorf("Error() =\n  %q\nwant\n  %q", got, want)
	}

	// An Input with no identity still produces a readable message.
	bare := unreachableError(Input{}, errors.New("boom"))
	if got, want := bare.Error(), "cannot reach cluster: boom"; got != want {
		t.Errorf("bare Error() = %q, want %q", got, want)
	}
}
