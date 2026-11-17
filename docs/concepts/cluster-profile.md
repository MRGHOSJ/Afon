# Cluster profile

`afon inspect` probes the cluster you point it at and prints a
**ClusterProfile** — a structured, read-only map of what the cluster
actually offers, so plans adapt to reality instead of assuming a blank
canvas.

```bash
afon inspect                       # current kubeconfig context
afon inspect --context k3d-dev     # a named context
afon inspect -n hello-app          # scope/anchor a namespace view
afon inspect -o json               # machine-readable
afon inspect --full                # include every section detail
afon inspect --redact              # mask identifiers and addresses
```

## What it probes

Sections include (availability varies by cluster):

- **version** — server version and reachable API.
- **nodes** — capacity, allocatable resources, conditions.
- **headroom** — scheduling room for the workloads you are about to
  plan (the "pre-flight guarantee": fail at plan time, not with
  Pending pods).
- **ingress** — ingress classes present (and whether a requested route
  has a class to serve it).
- **storage** — storage classes.
- **CRDs** — custom resource definitions installed.
- **RBAC** — what the current identity can actually do.

Each section reports a status:

| Section status | Meaning |
|---|---|
| `ok` | Probed successfully. |
| `denied` | The cluster refused (RBAC) — surfaced as data, not a crash. |
| `partial` | Some information obtained, some denied/missing. |

## Reading the output

Text mode groups sections with aligned rows:

```
version   ok      v1.31.4+k3s1
nodes     ok      1 ready, 2.9 GiB allocatable
headroom  ok      fits 12 × requests(100m, 128Mi)
ingress   partial no ingress class found (routes requesting one will plan as warnings)
```

`-o json` wraps the same structure for scripts; `--full` adds per-item
detail that the summary collapses; `--redact` masks identifiers and
network addresses (safe to paste into tickets).

## How it feeds the pipeline

The profile is the foundation for **pre-flight checks**: a plan can warn
*before applying* when the cluster cannot satisfy the flow — "route
requested but no ingress class", "apply needs `patch` but access denied",
"not enough schedulable headroom". Today `inspect` is the standalone
command; wiring profile data into plan-time warnings is in progress
(see the [Roadmap](../ROADMAP.md), Phase 3 leftovers).

## Read-only contract

`inspect` performs only reads (`get`/`list`/`watch`). It creates,
modifies, and deletes nothing — same guarantee as `plan`.

Next: [Security model](security-model.md).
