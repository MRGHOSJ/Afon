// Package k8s is the Phase 3 Kubernetes driver: one driver for k8s,
// k3s, and k3d, speaking only standard APIs (client-go dynamic client,
// server-side apply, label-scoped discovery). No flavor detection, no
// forked code paths — the API server is the compatibility layer.
//
// Plan is strictly read-only: classification rides dry-run applies
// (the kubectl-diff algorithm the Stage 2a spike ratified against a
// live cluster) and pruning is a label-scoped LIST. Apply rehearses
// every mutation with a dry-run before the first write, so any failure
// the server can predict — conflicts, admission denials, schema errors —
// happens before anything is written; only a race after rehearsal can
// leave a partial apply, and re-running converges.
//
// Ownership is two labels (afon.dev/flow, afon.dev/node) stamped into
// every applied body. Plan refuses objects another owner holds
// (driver.ErrForeignOwner) and prunes only objects whose labels match
// the caller exactly, re-verified client-side after the selector.
//
// Known limitations, documented for Phase 4 (inventory):
//   - a kind that disappears entirely from a flow is not pruned —
//     only kinds present in the desired set are listed;
//   - ownership is label-only at plan time; delete additionally pins
//     the UID observed during apply's validation pass, but there is no
//     plan-time UID verification;
//   - one namespace per driver; cluster-scoped objects are refused
//     with driver.ErrClusterScoped.
//
// The REST mapper is a snapshot taken at construction: CRDs installed
// afterwards do not resolve until the driver is rebuilt (for the CLI,
// the next invocation).
package k8s
