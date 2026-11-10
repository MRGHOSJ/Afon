package probe_test

import (
	"context"
	"errors"
	"net/url"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/MRGHOSJ/afon/pkg/probe"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

// TestProbeUnreachableRefused covers the hard-failure path end to end: the
// version probe fails with a refused connection and Probe must return the
// classified *UnreachableError instead of a profile.
func TestProbeUnreachableRefused(t *testing.T) {
	refused := &url.Error{
		Op:  "Get",
		URL: "https://127.0.0.1:53502/version",
		Err: &os.SyscallError{Syscall: "connectex", Err: syscall.ECONNREFUSED},
	}
	client := fake.NewClientset()
	client.PrependReactor("get", "version",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, refused
		})

	_, err := probe.Probe(context.Background(), probe.Input{
		Client:      client,
		RESTConfig:  &rest.Config{Host: "https://127.0.0.1:53502"},
		KubeContext: "k3d-dev",
		ClusterName: "k3d-dev",
		Namespace:   "default",
		Now:         func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) },
	})
	if err == nil {
		t.Fatal("Probe() error = nil, want *UnreachableError")
	}
	var ue *probe.UnreachableError
	if !errors.As(err, &ue) {
		t.Fatalf("Probe() error type = %T (%v), want *UnreachableError", err, err)
	}
	want := `cannot reach cluster "k3d-dev" (https://127.0.0.1:53502): connection refused — is the cluster running?`
	if got := err.Error(); got != want {
		t.Errorf("Probe() error =\n  %q\nwant\n  %q", got, want)
	}
}

// TestProbeUnreachableForbidden covers a reachable cluster that rejects the
// version read: same hard-failure path, different classification.
func TestProbeUnreachableForbidden(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: "version"}, "", errors.New("nope"))
	client := fake.NewClientset()
	client.PrependReactor("get", "version",
		func(action k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, forbidden
		})

	_, err := probe.Probe(context.Background(), probe.Input{
		Client:      client,
		RESTConfig:  &rest.Config{Host: "https://10.0.0.9:6443"},
		KubeContext: "prod",
		Namespace:   "default",
	})
	if err == nil {
		t.Fatal("Probe() error = nil, want *UnreachableError")
	}
	want := `not authorized for cluster "prod" (https://10.0.0.9:6443): version is forbidden: nope — check your kubeconfig credentials`
	if got := err.Error(); got != want {
		t.Errorf("Probe() error =\n  %q\nwant\n  %q", got, want)
	}
}
