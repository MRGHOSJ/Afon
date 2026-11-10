package probe

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// UnreachableError is the hard failure Probe returns when the version probe
// fails: the cluster could not be reached or could not be read at all. Every
// other section failure degrades to a partial profile instead. The Cause and
// Hint are classified from the original error so the message says what to do
// about it, not just what the dial stack printed.
type UnreachableError struct {
	Context string // kubeconfig context name, may be ""
	Server  string // API server URL, may be ""
	Cause   string // human-readable cause
	Hint    string // actionable suggestion, may be ""
	verb    string // "cannot reach" or "not authorized for"
}

func (e *UnreachableError) Error() string {
	verb := e.verb
	if verb == "" {
		verb = "cannot reach"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s: %s", verb, e.target(), e.Cause)
	if e.Hint != "" {
		b.WriteString(" — " + e.Hint)
	}
	return b.String()
}

// target names the cluster with whatever identity the Input carried, so a
// multi-context kubeconfig says which one failed.
func (e *UnreachableError) target() string {
	switch {
	case e.Context != "" && e.Server != "":
		return fmt.Sprintf("cluster %q (%s)", e.Context, e.Server)
	case e.Context != "":
		return fmt.Sprintf("cluster %q", e.Context)
	case e.Server != "":
		return fmt.Sprintf("cluster (%s)", e.Server)
	default:
		return "cluster"
	}
}

// unreachableError builds the hard-failure error from the probe input and
// the original version-probe failure.
func unreachableError(in Input, err error) *UnreachableError {
	verb, cause, hint := classifyUnreachable(err)
	e := &UnreachableError{Cause: cause, Hint: hint, verb: verb}
	if in.KubeContext != "" {
		e.Context = in.KubeContext
	}
	if in.RESTConfig != nil {
		e.Server = in.RESTConfig.Host
	}
	return e
}

// classifyUnreachable turns a version-probe failure into verb, cause and
// hint. Order matters: a server that rejects us is not a connectivity
// problem, so auth is checked before the network causes. Each branch matches
// on the error chain (errors.Is/As) with a string fallback for OS wordings
// that reach us already flattened.
func classifyUnreachable(err error) (verb, cause, hint string) {
	tlsMsg := tlsCause(err) // computed once; only used when it matches
	switch {
	case apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err):
		return "not authorized for", err.Error(), "check your kubeconfig credentials"
	case isConnectionRefused(err):
		return "cannot reach", "connection refused", "is the cluster running?"
	case isDNSFailure(err):
		return "cannot reach", "cannot resolve host", "check the server address in your kubeconfig"
	case isTimeout(err):
		return "cannot reach", fmt.Sprintf("timed out after %s", ProbeTimeout),
			"check the network path (firewall/VPN)"
	case tlsMsg != "":
		return "cannot reach", tlsMsg, "the certificate may have been reissued — refresh your kubeconfig"
	default:
		return "cannot reach", err.Error(), ""
	}
}

func isConnectionRefused(err error) bool {
	if errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	return strings.Contains(err.Error(), "refused")
}

func isDNSFailure(err error) bool {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	return strings.Contains(err.Error(), "no such host")
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return strings.Contains(err.Error(), "i/o timeout")
}

// tlsCause returns the TLS failure's own message, without the surrounding
// Get "url" transport wrapper, or "" when err carries no TLS failure.
func tlsCause(err error) string {
	var certVerify *tls.CertificateVerificationError
	if errors.As(err, &certVerify) {
		return certVerify.Error()
	}
	var unknownAuthority x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthority) {
		return unknownAuthority.Error()
	}
	var invalidCert x509.CertificateInvalidError
	if errors.As(err, &invalidCert) {
		return invalidCert.Error()
	}
	var hostnameErr x509.HostnameError
	if errors.As(err, &hostnameErr) {
		return hostnameErr.Error()
	}
	if strings.Contains(err.Error(), "x509:") {
		return err.Error()
	}
	return ""
}
