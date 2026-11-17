# Flow schema

The flow file format: `afon.dev/flow/v1`. One YAML document per file.

## Document identity

```yaml
apiVersion: afon.dev/flow/v1    # required, exact
kind: Flow                      # required, exact
metadata:
  name: my-app                  # required, non-empty
spec:
  target: { ... }               # optional block
  nodes: [ ... ]                # required, non-empty
```

Anything else (`apiVersion: v1`, `kind: Application`, missing
`metadata`) is rejected with a position: `flow.yaml:1:1: unsupported
apiVersion "v1", want "afon.dev/flow/v1"`.

## Top-level fields

| Field | Required | Type | Notes |
|---|---|---|---|
| `apiVersion` | yes | string | Must equal `afon.dev/flow/v1` |
| `kind` | yes | string | Must equal `Flow` |
| `metadata.name` | yes | string | Non-empty. Names release records: `afon.release.<name>.v<seq>` |
| `spec.target` | no | mapping | Execution target (see below) |
| `spec.nodes` | yes | list | Non-empty list of node mappings |

Unknown top-level keys are not part of the contract — keep the document
to the fields above.

## `spec.target`

| Field | Type | Notes |
|---|---|---|
| `target.context` | string | Kubeconfig context. **`--context` flag wins** when both are set. |
| `target.namespace` | string | The **one** namespace this flow manages. Required when using `--driver k8s` (the driver refuses flows without it). |

Both are optional for the fake driver (which has no namespaces). Afon
never writes to more than one namespace per flow; cluster-scoped objects
are refused.

## Node entries

```yaml
nodes:
  - id: build              # required
    type: image-build      # required — a registered type name
    needs: [src]           # optional — ordering without data flow
    onFailure: rollback    # optional — halt (default) | rollback
    config: { ... }        # type-specific; validated strictly
```

| Field | Required | Rules |
|---|---|---|
| `id` | yes | `^[A-Za-z0-9_-]+$`, unique within the flow |
| `type` | yes | Registered type (`image-ref`, `app-manifests`, `k8s-manifests`, `k8s-apply`, `fake-object`, `git-source`, `image-build`) |
| `needs` | no | List of node ids (same pattern). Creates ordering edges only. |
| `onFailure` | no | `halt` (default) or `rollback`. Anything else is rejected. |
| `config` | depends | Passed to the node type's validator. **Unknown fields are rejected.** Values may contain `${...}` references. |

### Node types and their configs

Documented per type with full tables and examples:
[Node types](node-types.md).

## References inside `config`

```yaml
config:
  image: ${build.ref}          # node output
  repo:  ${src.url}
  auth:  ${secret.registry-creds}   # reserved form
  note:  "built from ${src.sha}"    # interpolation inside a string
  literal: "$${NOT_A_REF}"          # escaped — stays literal ${NOT_A_REF}
```

- Extracted at parse time with positions (errors point at line:column).
- Validated against the producer's **declared outputs** at compile time.
- Whole-string refs preserve type; embedded refs interpolate as strings.

Full rules: [Reference grammar](reference-grammar.md).

## Complete examples

### Minimal (fake driver)

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
    - id: obj
      type: fake-object
      config:
        kind: ConfigMap
        name: hello
        data:
          digest: ${image.digest}
```

### Full pipeline (k8s)

```yaml
apiVersion: afon.dev/flow/v1
kind: Flow
metadata:
  name: git-build-demo
spec:
  target:
    context: k3d-dev          # optional — --context flag also works
    namespace: git-build-demo
  nodes:
    - id: src
      type: git-source
      config:
        url: https://github.com/your-org/your-repo.git
        ref: main
    - id: build
      type: image-build
      config:
        image: registry.example.com/your-org/your-app   # no tag
        repo: ${src.url}
        sha: ${src.sha}
        registrySecret: registry-creds
    - id: web
      type: app-manifests
      config:
        name: your-app
        image: ${build.ref}
        port: 8080
    - id: deploy
      type: k8s-apply
      config:
        manifests: ${web.rendered}
      onFailure: rollback
```

## Validation pipeline (what `afon validate` runs)

1. **Parse** (this schema): document identity, metadata, node shapes,
   duplicate ids, reference extraction — with source positions.
2. **Compile** (engine): registry lookup per `type`, per-node config
   validation, `${node.output}` checked against declared outputs,
   `needs`/ref edges → DAG → **cycle detection**.

Errors from either stage print `file:line:col: message`.

## Versioning

`apiVersion` is the wire format. A future `afon.dev/flow/v2` would be
opt-in — old files keep parsing. Node-type behavior versions live in
each type's `TypeVersion` (cache invalidation), not in the file format.
