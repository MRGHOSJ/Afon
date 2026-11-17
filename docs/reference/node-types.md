# Node types

Every registered node type: kind, config table, outputs, cache behavior,
and a working example. Configs are validated **strictly** — unknown
fields are rejected so typos fail at `afon validate`.

Legend for **Cluster:** whether apply needs `--driver k8s`.

---

## `image-ref` — action

Deterministic stand-in for an image reference. **Never pulls**: the
digest is `sha256("image-ref:" + ref)` — enough for cache keys,
substitution mechanics, and digest-pinning demos without a registry.

| Field | Type | Required | Notes |
|---|---|---|---|
| `ref` | string or ref | yes | Non-empty. Accepts `${...}`. |

**Outputs:** `ref`, `digest` · **Cluster:** no · **Cache:** config only

```yaml
- id: image
  type: image-ref
  config:
    ref: registry.example.com/your-org/app:1.0.0
```

---

## `app-manifests` — action

Generates a Deployment (and a Service when `port` is set) from a small
app spec — the "write a flow, not Kubernetes YAML" path. Output is
deterministic (canonical YAML, sorted keys). The namespace comes from
the flow target.

| Field | Type | Required | Notes |
|---|---|---|---|
| `name` | string | yes | DNS-1123 label (`[a-z0-9]([-a-z0-9]*[a-z0-9])?`, ≤63). **Literal** — no refs. |
| `image` | string or ref | yes | Container image. Accepts refs (`${build.ref}`). |
| `replicas` | int ≥ 0 | no | Default: emitted as generated default |
| `port` | int 1–65535 | no | When set, also generates a Service (80 → port). **Literal.** |
| `env` | mapping | no | Env vars: name → string or ref. Sorted by name in output. |
| `out` | string | no | Materialize the rendered YAML to this path (relative to the flow file). **Literal.** |

**Outputs:** `rendered` · **Cluster:** no · **Cache:** config only

```yaml
- id: web
  type: app-manifests
  config:
    name: transit
    image: ${build.ref}
    port: 8081
    env:
      LOG_LEVEL: info
    out: generated.yaml     # optional; pair with `afon render`
```

---

## `k8s-manifests` — action

Renders plain-Kubernetes YAML files into one canonical multi-document
stream for `k8s-apply`. No Kustomize, no overlays.

| Field | Type | Required | Notes |
|---|---|---|---|
| `paths` | list of strings/refs | yes | Non-empty. Non-recursive globs, **relative to the flow file's directory**, matched files sorted. |
| `images` | mapping | no | container-name → image (string or ref). Overwrites that container's `image` in every document where it appears. |

Behavior notes:

- Each document is re-marshaled with sorted keys (deterministic).
- The **target namespace is injected** into documents that don't declare
  one; a declared namespace is left alone (the driver rejects one that
  disagrees with the target rather than rewriting it).

**Outputs:** `rendered` · **Cluster:** no · **Cache:** config + file
contents (paths are file-dependent inputs)

```yaml
- id: manifests
  type: k8s-manifests
  config:
    paths:
      - deploy/*.yaml
    images:
      app: ${image.ref}     # replaces container named "app"
```

---

## `k8s-apply` — resource

Turns a rendered multi-document stream into desired objects for the
k8s driver. Identity = each document's `apiVersion`/`kind`/
`metadata.name`. Never "runs" — it diffs (server-side dry-run) and
applies (server-side apply with Afon's field manager).

| Field | Type | Required | Notes |
|---|---|---|---|
| `manifests` | string or whole-string ref | yes | The YAML stream — normally `${node.rendered}`. |

**Outputs:** none · **Cluster:** plan/apply with `k8s` (fake driver
refuses) · **Cache:** resources are never cached (always diffed)

```yaml
- id: deploy
  type: k8s-apply
  config:
    manifests: ${manifests.rendered}
  onFailure: rollback
```

Constraints: cluster-scoped objects are refused; objects owned by
another flow are refused; prune only touches this flow's objects.

---

## `fake-object` — resource

The Phase-2 fake-driver resource: one object the fake driver holds.
Exercises reference substitution, diffing (`create`/`update`/`none`),
and plan/apply without a cluster.

| Field | Type | Required | Notes |
|---|---|---|---|
| `kind` | string or ref | yes | e.g. `ConfigMap` |
| `name` | string or ref | yes | Object name |
| `namespace` | string or ref | no | |
| `data` | mapping or ref | no | Field values may be refs |

**Outputs:** none · **Cluster:** fake driver only · **Cache:** never
(diffed every plan)

```yaml
- id: obj
  type: fake-object
  config:
    kind: ConfigMap
    name: app-digest
    data:
      digest: ${image.digest}
```

---

## `git-source` — action

Resolves a git ref to a commit SHA. Plan **and** apply call
`git ls-remote` (a network failure degrades to `will-run` with the
reason at plan, and fails the node at apply). A full 40- or 64-hex ref
is already a SHA and skips the network entirely.

| Field | Type | Required | Notes |
|---|---|---|---|
| `url` | string or ref | yes | Repository URL (https/ssh as git supports) |
| `ref` | string or ref | yes | Branch/tag/SHA. **No whitespace.** |

**Outputs:** `sha`, `url` · **Cluster:** no (git network only) ·
**Cache:** external input — the **resolved SHA** rides the cache key, so
a moved branch invalidates downstream automatically (`InputCacher`).

```yaml
- id: src
  type: git-source
  config:
    url: https://github.com/your-org/your-repo.git   # placeholder
    ref: main
```

---

## `image-build` — action (cluster)

Runs a **throwaway Kubernetes Job** with pinned kaniko inside your
cluster, pushes `<image>:<git-sha>`, and publishes the pushed digest.
Tagless by design — `:latest` is impossible; the tag is always the
resolved git SHA.

| Field | Type | Required | Notes |
|---|---|---|---|
| `image` | string or ref | **yes** | Registry path **without tag** (enforced). |
| `repo` | string or ref | **yes** | Usually `${src.url}`. |
| `sha` | string or ref | **yes** | Usually `${src.sha}` — becomes the tag. |
| `registrySecret` | string or ref | **yes** | Secret **name** (never read by Afon) in the build namespace; mounted for push auth. |
| `gitSecret` | string or ref | no | Secret name mounted for private git checkout. |
| `namespace` | string or ref | no | Build namespace (default `afon-build`, auto-created with PSA baseline labels). |
| `dockerfile` | path | no | Relative path, default `Dockerfile`. |
| `context` | path | no | Relative build context, default `.`. |
| `target` | string or ref | no | Multi-stage build target. |
| `args` | mapping | no | Build args (key → string/ref). Keys: no `=`/whitespace. |
| `platform` | string or ref | no | e.g. `linux/amd64`. |
| `resources` | mapping | no | `{requests\|limits: {cpu\|memory: <quantity>}}`. |
| `timeout` | duration string | no | Positive Go duration (e.g. `20m`), default `20m`. |

**Outputs:** `digest`, `ref` (digest-pinned reference for deploys) ·
**Cluster:** **yes** — apply requires `--driver k8s`; plan-time cache
hits and registry matches never touch the cluster · **Cache:** config +
external inputs (`sha`), plus the **anonymous registry pre-check** on
cache miss (see [Caching](../concepts/caching.md)).

```yaml
- id: build
  type: image-build
  config:
    image: registry.example.com/your-org/your-app   # placeholder, no tag
    repo: ${src.url}
    sha: ${src.sha}
    registrySecret: registry-creds
```

Mechanics worth knowing:

- Job runs in the build namespace under PSA **baseline**
  (RuntimeDefault seccomp, no privilege); pinned builder images.
- On push, kaniko stamps `afon.dev/build-config=<content-key>` as an
  image label — the basis of the registry pre-check.
- Failure surfaces pod events (e.g. `FailedMount: secret … not found`),
  never Secret contents.
- No-cluster entry points fail clearly:
  `image-build: requires the k8s driver (apply only)`.

---

## Type summary

| Type | Kind | Outputs | Cluster | Cache |
|---|---|---|---|---|
| `image-ref` | action | `ref`, `digest` | no | config |
| `app-manifests` | action | `rendered` | no | config |
| `k8s-manifests` | action | `rendered` | no | config + files |
| `git-source` | action | `sha`, `url` | git only | config + resolved SHA |
| `image-build` | action | `digest`, `ref` | **yes** | config + SHA (+ registry pre-check) |
| `k8s-apply` | resource | — | yes | never diffed from cache |
| `fake-object` | resource | — | fake only | never diffed from cache |

Planned (not built): `trivy-scan` (severity threshold, allowlist,
DB-version/TTL cache dimension), Helm renderer, notify-webhook — see the
[Roadmap](../ROADMAP.md).

Want to add your own? [Node authoring](node-authoring.md).
