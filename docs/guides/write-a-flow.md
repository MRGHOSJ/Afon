# Write a flow

This is the authoring guide: the file format end-to-end, the loop you
iterate in, and every error a first-time writer hits.

## The file

`flow.yaml` — one document, this exact identity:

```yaml
apiVersion: afon.dev/flow/v1
kind: Flow
metadata:
  name: my-app                 # used in release records: afon.release.my-app.v1
spec:
  target:                      # optional for the fake driver; required (ns) for k8s
    context: k3d-dev           # optional: kubeconfig context (or pass --context)
    namespace: my-app          # the ONE namespace this flow manages
  nodes:
    - id: web                  # [A-Za-z0-9_-]+, unique
      type: app-manifests      # a registered node type
      config:
        name: web
        image: nginx:1.27-alpine
        port: 8080
    - id: deploy
      type: k8s-apply
      config:
        manifests: ${web.rendered}
      onFailure: halt          # halt (default) | rollback
```

### Fields

| Field | Required | Notes |
|---|---|---|
| `apiVersion` | yes | Must be `afon.dev/flow/v1` |
| `kind` | yes | Must be `Flow` |
| `metadata.name` | yes | Non-empty; names your release records |
| `spec.target.context` | no | Overridden by `--context` |
| `spec.target.namespace` | k8s: yes | The single managed namespace |
| `spec.nodes` | yes | Non-empty list |
| `nodes[].id` | yes | Letters, digits, `-`, `_` — unique |
| `nodes[].type` | yes | Registered type name |
| `nodes[].config` | depends | Validated by the node type; unknown fields are **rejected** |
| `nodes[].needs` | no | List of node ids for ordering without data flow |
| `nodes[].onFailure` | no | `halt` (default) or `rollback` |

Full field reference: [Flow schema](../reference/flow-schema.md).

## The authoring loop

```bash
afon validate -f flow.yaml     # 1. parse + compile: schema, types, refs, cycles
afon render  -f flow.yaml      # 2. materialize action outputs (if you use out:)
afon plan    -f flow.yaml      # 3. read-only diff (add --driver k8s --context X)
afon apply   -f flow.yaml      # 4. execute + record
```

Rules of thumb:

- **validate first** — it touches nothing and catches 90% of mistakes
  with `file:line:col` positions.
- **plan is your review step** — it can never change anything.
- **render is explicit** — `plan`/`apply` read `generated.yaml` as-is;
  they never silently regenerate it.

## Connecting nodes

```yaml
config:
  image: ${build.ref}          # consume an output (creates the edge)
needs: [build]                 # order only, no data
repo:  ${src.url}              # whole-string reference preserves type
tag:   build-${src.sha}        # interpolation inside a longer string
```

- Outputs are checked against the producer's **declared outputs** at
  compile time — `${build.sha}` fails immediately (image-build publishes
  `digest`/`ref`, not `sha`).
- Cycles are rejected with the cycle path.
- See [Reference grammar](../reference/reference-grammar.md) for escapes
  (`$${`) and reserved forms (`${secret.NAME}`, `${env.NAME}`).

## Worked mini-example (fake driver, no cluster)

```yaml
apiVersion: afon.dev/flow/v1
kind: Flow
metadata:
  name: hello
spec:
  nodes:
    - id: image
      type: image-ref
      config:
        ref: registry.example.com/your-org/app:1.0.0
    - id: cfg
      type: fake-object
      config:
        kind: ConfigMap
        name: hello
        data:
          digest: ${image.digest}
```

```bash
afon validate -f flow.yaml && afon plan -f flow.yaml && afon apply -f flow.yaml
```

## Worked example (k8s)

```yaml
apiVersion: afon.dev/flow/v1
kind: Flow
metadata:
  name: web
spec:
  target:
    namespace: web
  nodes:
    - id: manifests
      type: k8s-manifests
      config:
        paths: [deploy/*.yaml]          # globs, relative to this file
        images:
          app: registry.example.com/your-org/app:1.0.0
    - id: deploy
      type: k8s-apply
      config:
        manifests: ${manifests.rendered}
```

```bash
afon plan  -f flow.yaml --driver k8s --context k3d-dev
afon apply -f flow.yaml --driver k8s --context k3d-dev
```

## Common errors and fixes

| Error (abridged) | Cause | Fix |
|---|---|---|
| `unsupported apiVersion "v1"` | Wrong apiVersion | Use `afon.dev/flow/v1` |
| `missing metadata.name` | Empty/absent name | Add a non-empty `metadata.name` |
| `spec.nodes must be a non-empty list` | No nodes | Add at least one node |
| `invalid node id "my node"` | Space in id | Only `[A-Za-z0-9_-]` |
| `duplicate node id "web"` | Reused id | Rename one |
| `node "x" is missing type` | Forgot `type` | Add the registered type name |
| `unknown field "path"` (in config) | Typo in config | The node type rejects unknown fields — check [Node types](../reference/node-types.md) |
| `missing required field "url"` | Incomplete config | See the node's config table |
| `invalid reference ${...}: want ${node.output}, ${secret.NAME}, or ${env.NAME}` | Malformed ref | One dot, non-empty sides; escape literals as `$${` |
| `unknown output "shas" of node "build"` | Typo'd output | Use a declared output (`digest`, `ref`, …) |
| `dependency cycle: a → b → a` | Cyclic refs/needs | Break the cycle |
| `image must not include a tag` | Tag in `image-build` config | Tagless by design — the tag is the git SHA |
| `create it before applying` (plan, k8s) | Target namespace missing | `kubectl create namespace …` or just `apply` (it creates it) |
| `ref must not contain whitespace` | Space in git ref | Fix the ref |

Error positions are real: `flow.yaml:12:9: unknown field "path"` points
at the offending line and column.

## Checklist before you apply

- [ ] `afon validate` passes
- [ ] `plan` output shows what you expect (statuses, no surprise deletes)
- [ ] Placeholder images/repos swapped for real ones if you actually want
      to pull/push (see [Recipes](recipes.md))
- [ ] For `image-build`: the `registrySecret` exists in `afon-build`
- [ ] You are on the right `--context` (the plan header says so)

Next: [Recipes](recipes.md).
