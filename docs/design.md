# Afon design doc

Status: draft v1 · Scope: the thin slice (prebuilt image → plan → apply → wait → release → rollback)

Afon is a remote, cluster-aware delivery pipeline for Kubernetes and k3s. This document fixes four decisions the engine depends on: how nodes behave (action vs. resource), what the cluster profile contains, what a release record stores, and what a flow file looks like. Anything not covered here is out of scope for v1.

## 1. Action vs. resource model

Every node in a flow is one of two kinds. The kind belongs to the **node type**, registered in code. Users never write it in YAML.

| | Action | Resource |
|---|---|---|
| Examples | `git-source`, `image-build`, `trivy-scan`, `image-ref` | `k8s-apply` |
| Meaning | Runs something, produces outputs (digest, commit SHA, scan result) | Describes objects that should exist in the cluster |
| Identity | Content-addressed: hash of its inputs | Name and namespace in the cluster, tracked by an inventory |
| Plan means | Compute the cache key and compare it to the last release: `cached` or `will run` | Server-side dry-run apply, then diff against live objects: `create`, `update`, `none`, `delete` |
| Skipped when | Key matches and outputs are still valid | No diff |

**Plan is read-only.** It never executes an action. It only reports whether one would run, which keeps the preview trustworthy.

**Cache key for actions:**

```
key = sha256(canonicalJSON{ type, typeVersion, resolvedConfig, upstreamOutputs })
```

- Canonical JSON (sorted keys, no whitespace), or equal inputs hash differently between runs.
- `typeVersion` is included so a change to a node's code invalidates its cache.
- A cache hit is **verified**, not trusted. A recorded image digest is only valid if the registry still has it.
- Scans carry a time or database dimension (Trivy DB version, or a 24h TTL). An unchanged image can gain new CVEs, so a cached "clean" result must expire.
- Secrets enter the key as a reference name, never as a value.

**Outputs feed resources.** `image-build` outputs a digest, and the manifests consume it. Deploys are by digest, never by tag, so a rollback restores exactly what ran before.

**Execution:** nodes whose dependencies are complete run in parallel (`errgroup`). On a failure the executor stops scheduling, lets in-flight nodes finish or cancel, and records exactly what succeeded. No release is marked `deployed` unless the flow completed.

## 2. ClusterProfile schema (`afon.dev/probe/v1`)

The profile is **facts only**: "ingress classes: traefik (default)", never "routing is fine". Judgments belong in a separate `pkg/checks` layer that consumes saved profiles. Implemented in `pkg/probe`.

```go
type ClusterProfile struct {
    SchemaVersion string        // "afon.dev/probe/v1"
    GeneratedAt   time.Time     // UTC
    DurationMs    int64
    Identity      Identity      // kubeContext, clusterName, server, clusterUID,
                                // flavor{name, versionSuffix, providerID, hints, environment}
    Version       VersionInfo   // server version, client-go version, minorDelta, withinSkewWindow
    Discovery     DiscoveryInfo // group/resource counts + capabilities (traefik, gatewayAPI,
                                // certManager, argoCD, flux, kyverno, metricsServer, ...)
    Health        HealthInfo    // ready, failed readyz checks, CoreDNS, kube-system components
    Nodes         NodesInfo     // per node facts + capacity, requested, schedulingHeadroom
    Access        AccessInfo    // SSRR rules + 32 curated SSAR checks for the target namespace
    Namespace     NamespaceInfo // exists, phase, PSA labels, quotas, limit ranges
    Networking    NetworkingInfo// ingressClasses, gatewayAPI, networkPolicies, cniHints
    Storage       StorageInfo   // storageClasses, PVs, PVCs
    Sections      map[string]SectionStatus
    Errors        []ProbeError
}
// Planned optional sections (pointers, nil when the component is absent):
// TLS (cert-manager issuers), GitOps (Argo CD/Flux scope), Registry, ControlPlaneCerts, Policy.
```

Conventions that every consumer relies on:

- **Section status:** `ok | partial | denied | unavailable`, classified once in the composer from the probe's errors (forbidden → denied, not found or no kind match → unavailable, mixed results → partial).
- **`[]` vs. `null`:** an empty list means "probed, found none". Null or absent means "not probed". Optional components are nil when not installed.
- **`ProbeError`:** `{section, resource, reason, code}`, never a bare string.
- **Namespace-scoped by default.** Cluster-wide listing is opt-in. The one exception is the cluster-wide pod list used for requested-vs-allocatable, which degrades to `partial` if denied.
- **`schedulingHeadroom` is allocatable minus pod requests.** It is not usage.
- **Safe to save:** labels and annotations are allowlisted, `k3s.io/node-args` and `node-env` are never emitted, and `--redact` masks identifiers and addresses.

## 3. Release record (`afon.dev/release/v1`)

One immutable record per apply attempt, stored as a Secret in the target namespace (the approach Helm uses), compressed.

```
name:   afon.release.<flow>.v<N>
labels: afon.dev/flow, afon.dev/revision, afon.dev/status
data:   release = gzip(JSON(Release))
```

```go
type Release struct {
    APIVersion   string         // afon.dev/release/v1
    Flow         string
    Revision     int            // monotonic, never reused
    Status       string         // pending | deployed | failed | superseded | rolledback
    CreatedAt    time.Time
    FlowHash     string         // hash of the flow file as applied
    RolledBackTo *int
    Target       TargetRef      // context, namespace, cluster UID
    Actions      []ActionRecord // nodeId, cache key, outputs (digests, SHAs), cached?
    Manifests    []byte         // rendered YAML, secret data stripped
    Inventory    []ObjectRef    // group, version, kind, namespace, name, UID
    Profile      *ProfileDigest // small summary: flavor, version, detected components
}
```

Rules:

- **Append-only.** A rollback writes revision N+1 with `rolledBackTo: K`. Nothing is overwritten.
- **`pending` is written before apply starts.** A `pending` record with no live run means a crashed run, which the next run detects and reports. A non-stale `pending` record also blocks concurrent applies.
- **Inventory drives pruning.** Delete only objects in the previous inventory that are absent from the new one, matched by UID as well as name. Never delete anything outside the inventory, and never match by label alone.
- **Never stored:** secret values, kubeconfig contents, tokens, registry credentials, or the full profile.
- **Limits:** a Secret holds at most 1 MiB, so payloads are gzipped, failures are loud, and old revisions are pruned (default keep 10). A later `Backend` implementation can store releases elsewhere.

**Rollback** re-applies release K's manifests and digests (dry-run plan first), prunes by inventory, and writes a new revision. It does **not** undo database migrations, volume contents, CRD schema changes, or effects outside the cluster, and the rollback plan says so. Auto-rollback after a failed readiness gate uses the same path.

## 4. Flow YAML schema (`afon.dev/flow/v1`)

```yaml
apiVersion: afon.dev/flow/v1
kind: Flow
metadata:
  name: hello
spec:
  target:
    context: k3d-dev
    namespace: hello
  nodes:
    - id: image
      type: image-ref
      config: { ref: docker.io/library/nginx:1.27 }
    - id: manifests
      type: k8s-manifests
      config:
        paths: [./deploy]
        images: { app: ${image.ref} }
    - id: apply
      type: k8s-apply
      config:
        manifests: ${manifests.rendered}
        wait: { timeout: 120s }
      onFailure: rollback
```

```go
type Node struct {
    ID        string         `yaml:"id"`
    Type      string         `yaml:"type"`
    Needs     []string       `yaml:"needs,omitempty"`     // only for ordering with no data dependency
    Config    map[string]any `yaml:"config,omitempty"`
    OnFailure string         `yaml:"onFailure,omitempty"` // "halt" (default) | "rollback"
}
```

- **References:** `${nodeID.outputName}`. Each reference adds a DAG edge. Node types declare their output names, so unknown nodes, unknown outputs, and cycles are errors at `validate` time, with line numbers.
- **Reserved now, implemented later:** `${secret.NAME}` and `${env.NAME}`, resolved at apply time.
- **Rendering is separate from applying.** `k8s-manifests` renders (Kustomize and Helm become other renderers) and `k8s-apply` applies. Image override is by container name, not YAML templating.
- **Ownership:** a flow owns the objects it labels. The plan refuses to touch objects owned by another flow.

## 5. Open questions

1. Name and schema group: the code uses `afon`, and it ends up in labels and API versions, so settle it before the first release.
2. Hooks (migrations that must finish before a Deployment): excluded from v1, with `needs` as the likely mechanism.
3. Release storage: in-cluster Secrets now. Revisit an external backend if cluster loss matters.
4. Where the engine/driver boundary sits for readiness: the engine owns the gate policy, and the driver reports status.
