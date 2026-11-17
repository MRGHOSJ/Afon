# Dependencies and outputs

Nodes communicate through **outputs**. A reference to an output is both a
data edge (ordering) and an input to caching (value hashing).

## Reference grammar

Three forms exist:

| Form | Meaning |
|---|---|
| `${nodeId.output}` | Another node's output. The canonical form. |
| `${secret.NAME}` | Reserved: a credentials Secret name (referenced, never read by Afon). |
| `${env.NAME}` | Reserved: an environment value. |

Rules:

- Exactly one dot per regular reference; neither side may contain a dot
  or be empty (`${a.b.c}` is invalid).
- `$${` escapes a literal `${`.
- **Whole-string references** (`ref: ${build.ref}`) preserve the value's
  type; **interpolations** (`image: prefix-${x}`) are string
  interpolation.
- References are extracted from `config` at parse time with source
  positions — errors print `flow.yaml:12:9`.

Full rules: [Reference grammar](../reference/reference-grammar.md).

## Declared outputs

Each node type declares the output names it produces; Compile checks
every `${node.output}` against that list **before anything runs**:

| Node | Outputs |
|---|---|
| `git-source` | `sha`, `url` |
| `image-build` | `digest`, `ref` |
| `image-ref` | `ref`, `digest` |
| `app-manifests`, `k8s-manifests` | `rendered` |
| resource nodes (`k8s-apply`, `fake-object`) | *(none)* |

A typo like `${build.shas}` fails at compile time, not at 2 a.m.

## Building the graph

Edges come from references automatically. For ordering **without** data
flow, declare it explicitly:

```yaml
- id: scan
  type: ...
  needs: [build]    # runs after build, but scan doesn't consume its outputs
```

`needs:` entries must be valid node ids (`[A-Za-z0-9_-]+`), and cycles
are rejected with the cycle path.

## Execution semantics

- **Topological order**, run with bounded parallelism
  (`--parallelism`, default 4). Independent nodes run concurrently.
- **Halt on failure**: a failed node stops scheduling new nodes;
  in-flight nodes are allowed to finish or are cancelled by Ctrl-C.
- **Outputs are only visible to dependents** — plan-time downstream
  nodes that can't yet compute their key report
  `(inputs unknown until "X" runs)` rather than guessing.

## An example graph

```yaml
nodes:
  - id: src
    type: git-source
    config: {url: https://github.com/your-org/your-repo.git, ref: main}
  - id: build
    type: image-build
    config:
      image: registry.example.com/your-org/your-app
      repo: ${src.url}          # edge: src → build (data)
      sha: ${src.sha}           # edge: src → build (data)
      registrySecret: registry-creds
  - id: web
    type: app-manifests
    config:
      name: your-app
      image: ${build.ref}       # edge: build → web (digest-pinned image)
      port: 8080
  - id: deploy
    type: k8s-apply
    config:
      manifests: ${web.rendered} # edge: web → deploy (data)
```

Chain: `src → build → web → deploy`. Add `needs: [build]` to an
unrelated node to sequence it without consuming outputs.

## Failure propagation

If `build` fails:

- `web` and `deploy` never run (no inputs).
- The apply records `failed` and inventories whatever it did apply.
- With `onFailure: rollback` on a node, the run auto-rolls back — see
  [Releases and rollback](releases-and-rollback.md).

Next: [Releases and rollback](releases-and-rollback.md).
