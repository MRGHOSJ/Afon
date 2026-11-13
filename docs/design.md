# Afon design doc

Status: draft v3 · Scope: the thin slice (prebuilt image → plan → apply → wait → release → rollback), plus the apply safeguards settled before release records

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
key = sha256(canonicalJSON{ type, typeVersion, resolvedConfig })
```

- Canonical JSON (sorted keys, no whitespace, numbers normalized so `1` and `1.0` hash alike), or equal inputs hash differently between runs.
- `resolvedConfig` has all `${...}` references substituted with their resolved values, so upstream outputs enter the key only through what this node actually references (rule 3 below).
- `typeVersion` is included so a change to a node's code invalidates its cache.
- A cache hit is **verified**, not trusted. A recorded image digest is only valid if the registry still has it.
- Scans carry a time or database dimension (Trivy DB version, or a 24h TTL). An unchanged image can gain new CVEs, so a cached "clean" result must expire.
- Secrets enter the key as a reference name, never as a value.

**Outputs feed resources.** `image-build` outputs a digest, and the manifests consume it. Deploys are by digest, never by tag, so a rollback restores exactly what ran before.

**Execution:** nodes whose dependencies are complete run in parallel (`errgroup`). On a failure the executor stops scheduling, lets in-flight nodes finish or cancel, and records exactly what succeeded. No release is marked `deployed` unless the flow completed.

**Node interfaces (implemented in `pkg/engine`).** The kind belongs to the type, so the contract splits:

```go
type Base interface {
    Type() string; TypeVersion() string; Kind() Kind
    Validate(cfg Config) error
    DeclaredOutputs() []string
}
type ActionType  interface { Base; Run(ctx, cfg Config, in Inputs) (Outputs, error) }
type ResourceType interface { Base; Desired(ctx, cfg Config, in Inputs) ([]driver.Object, error) }
```

Only `ActionType.Run` has side effects. The engine owns the cache check (compute key, ask the store, mark `cached` or call `Run`) and owns every driver call (`Desired` only declares what should exist). Plan is read-only by construction: it never calls `Run`.

### Phase 2 engine rules

These are settled decisions; each exists to stop a later accident.

1. **`will-run` at plan time is an upper bound.** If an action's inputs were unknown during planning, its key is computed at apply time and may hit the cache then. Apply reports what actually happened (`NodeResult.Cached`). A plan can predict more work than the apply performs, never less.
2. **Unknown poisons the whole value and the whole node, not a field.** Interpolating an Unknown into a longer string makes that string Unknown. A resource node with any Unknown value anywhere in its config is `known-after-apply`. Less precise than Terraform, but simple and correct.
3. **Hash only what the node references.** The key is `sha256(canonicalJSON{type, typeVersion, resolvedConfig})` — `resolvedConfig` already embeds referenced upstream values. Hashing all of `upstreamOutputs` would let a `needs`-only edge or an unrelated output invalidate the cache for no reason.
4. **Cache writes happen only after a successful `Run`.** A failed action never leaves an entry behind.
5. **`validate` means schema plus Compile.** Types, declared outputs, and cycles only surface in Compile, so `afon validate` runs it with the registry populated explicitly in `cmd`. It never touches a driver.
6. **The fake driver has its own mutex; `Apply` is atomic per call.** Parallel resource nodes call it concurrently: build the new state in memory, write the file once with temp-and-rename. A failure midway leaves the old file untouched. Two processes running at once are still unprotected — a known gap until file locking exists.
7. **`Object.Data` is normalized exactly like hashing input.** Diffs compare canonical forms, so `1` vs `1.0` and map ordering can never produce phantom updates.

### Reference grammar (`${...}`)

- Node IDs and output names exclude dots, so `${a.b}` is unambiguous: `a` is the node, `b` is the output.
- A **whole-string** reference (`${image.ref}` as the entire scalar) keeps the referenced value's type — object, number, bool.
- **Interpolation** inside a longer string (`"repo/${src.sha}"`) always yields a string.
- References nest inside maps and lists; each one adds a DAG edge.
- `$${` escapes a literal `${`.
- `${secret.NAME}` and `${env.NAME}` parse, but a node that needs one is an error in Phase 2 (resolved at apply time, Phase 3+).

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
      onFailure: rollback
```

(Wait is not a per-node key: `k8s-apply` accepts only `manifests`. Readiness waiting is engine-level — `--wait-timeout` / `--no-wait`, see section 5. `onFailure: rollback` is Phase 4.)

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

## 5. Apply safeguards

These decisions landed with the apply-safeguard stage; each exists to stop a specific accident.

1. **Readiness waiting is driver-reported, engine-gated.** The engine decides *whether* to wait (`ExecutorOptions.Wait` / `WaitTimeout`, fed by the `--wait-timeout` and `--no-wait` flags); the driver reports *when* objects are ready through the additive `driver.Waiter` interface. `timeout <= 0` means no deadline — the context bounds the wait (kubectl semantics). The engine waits only for objects its own create/update diffs produced, and a wait failure fails the node (`wait: …`) before any release could be marked deployed. Unknown kinds are skipped, an object that disappears during the wait is an error, and a `Failed` Job fails immediately with its condition message.
2. **The delete prompt counts objects, not plan entries.** `Plan.DeleteObjectCount()` walks every entry's `OpDelete` diffs; the plan summary counts entries by status. One resource node whose worst op is delete contributes 1 entry regardless of batch size, so reading the summary would undercount — in the worst case telling the operator "1 object" while two die. The prompt and the non-TTY refusal both use the object count.
3. **The prompt only covers plan-visible deletes.** A chain that becomes destructive only after an upstream action runs (known-after-apply) can delete unprompted. Materialized flows — where `render` freezes every input before `plan` — give the prompt full coverage; that is the recommended shape for destructive changes.
4. **Kind-removal leaves orphans.** Prune lists only kinds present in the desired set (label-scoped LIST per GVR). A Service whose kind vanishes entirely from the flow is never listed: no plan entry, no prompt, no deletion — it is orphaned, not silently destroyed. Phase 4's inventory closes this.
5. **Three count bases, each labeled.** Plan text entry suffix `N changes` (diff count — honest for mixed create/delete batches), summary footer `N delete entry` (entry count; only the delete token carries the ` entry` suffix because it is the safety-relevant count — other tokens remain bare entry counts by design), prompt `N object(s)` (actual OpDelete objects). Three bases, three labels, no number reused for two meanings.
6. **Header vs banner — both exist on purpose.** Text mode prints a header on stdout after planning (`driver: k8s context: … namespace: … force: … prune: …`); a one-line banner on stderr prints after config parsing but *before* connecting. The banner is the only context signal in `-o json` mode (the header is text-only; Plan JSON carries no target) and when discovery fails (the header never prints). Known limitation: the header's k8s form is verified by unit test (`TestPrintPlanHeader`) and live demo only — hermetic end-to-end verification needs a fake discovery path, deferred.
7. **`render --check` is an exit-code contract.** Stale/edited/missing files → exit 1 with `render --check: N file(s) not up to date`; stdout keeps printing the would-be content (the `terraform fmt -check` style), so a CI log shows the diff that was refused.

## 6. Open questions

1. Name and schema group: the code uses `afon`, and it ends up in labels and API versions, so settle it before the first release.
2. Hooks (migrations that must finish before a Deployment): excluded from v1, with `needs` as the likely mechanism.
3. Release storage: in-cluster Secrets now. Revisit an external backend if cluster loss matters.
4. ~~Where the engine/driver boundary sits for readiness~~ — **resolved:** the engine owns the gate policy (`ExecutorOptions.Wait`), the driver reports status (`driver.Waiter`).
