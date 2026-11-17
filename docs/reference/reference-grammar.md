# Reference grammar

The `${...}` grammar: how values flow between nodes, and the exact rules
the parser enforces.

## The three forms

| Form | Kind | Meaning |
|---|---|---|
| `${nodeId.output}` | regular | Another node's declared output |
| `${secret.NAME}` | reserved | A credentials Secret **name** (referenced; Afon never reads it) |
| `${env.NAME}` | reserved | An environment value |

`NAME` in reserved forms may not contain dots. Regular references have
**exactly one dot**; both sides must match `^[A-Za-z0-9_-]+$` and be
non-empty.

```
${build.ref}              ✓
${src.sha}                ✓
${secret.registry-creds}  ✓
${env.HOME}               ✓
${a.b.c}                  ✗ two dots
${build.}                 ✗ empty output
${.ref}                   ✗ empty node
${build ref}              ✗ space
```

Malformed references fail with the position and the full rule:

```
flow.yaml:14:11: invalid reference ${build.ref.x}: want ${node.output}, ${secret.NAME}, or ${env.NAME}
```

## Whole-string vs interpolation

| Style | Behavior |
|---|---|
| **Whole-string** — the scalar is *exactly* one reference: `image: ${build.ref}` | The referenced value keeps its **type** (number stays number, etc.) |
| **Interpolation** — reference embedded in a longer string: `image: registry/${x}` | Result is a string; all refs are substituted |

An unterminated reference (`${build.ref` with no `}`) is a parse error
with position.

## Escaping

`$${` produces a literal `${` and starts **no** reference:

```yaml
config:
  pattern: "$${NOT_A_REF}"    # literal string: ${NOT_A_REF}
```

## Where references may appear

Inside a node's `config` subtree, in **any scalar string value** —
mapping values, sequence items, nested structures. Extraction happens at
parse time, in document order, and each carries the position of its
scalar.

The node type decides which *resolved* fields it accepts: most accept
refs anywhere a string is allowed; a few fields are deliberately
**literal-only** because materialization or object identity must be
predictable:

| Literal-only field | Why |
|---|---|
| `app-manifests.name`, `.port`, `.replicas`, `.out` | Object identity + materialized path must be knowable before Run |
| `git-source.ref` whitespace rule | git refs cannot contain whitespace |

(Compile-time validation catches a ref in a literal-only field with a
clear type error.)

## Resolution order

1. **Parse** — extract all `${...}` with positions (this page's rules).
2. **Compile** — resolve each regular ref against the producer's
   **declared outputs**; unknown outputs fail immediately:
   ```
   node "web": unknown output "shas" of node "build"
   ```
3. **Plan** — upstream outputs may be unknown yet; downstream nodes
   report `(inputs unknown until "X" runs)` instead of guessing.
4. **Apply/Run** — values are substituted before the node's `Run` /
   `Desired` sees the config.

`secret.*` and `env.*` are resolved by the engine's reserved handling —
`secret.*` yields a **name** (mount reference), not secret data. Afon
has no code path that reads a credentials Secret's contents.

## Validation errors you may see

| Message (abridged) | Meaning |
|---|---|
| `invalid reference ${…}: want ${node.output}, ${secret.NAME}, or ${env.NAME}` | Grammar violation |
| `unterminated reference starting at ${…` | Missing `}` |
| `unknown output "x" of node "y"` | Producer doesn't declare it |
| `unknown node "y"` (via missing dependency) | Reference/`needs` to a non-existent id |
| `dependency cycle: …` | Refs/`needs` form a loop — cycle path printed |

## Worked example

```yaml
nodes:
  - id: src
    type: git-source
    config:
      url: https://github.com/your-org/your-repo.git
      ref: main
  - id: build
    type: image-build
    config:
      image: registry.example.com/your-org/your-app
      repo: ${src.url}          # whole-string → type preserved
      sha: ${src.sha}
      registrySecret: registry-creds
  - id: web
    type: app-manifests
    config:
      name: your-app
      image: ${build.ref}       # digest-pinned string from upstream
      port: 8080
      env:
        GIT_SHA: ${src.sha}     # interpolation target in a mapping value
        LABEL: "app-${src.sha}" # interpolation inside a longer string
```

Edges created: `src → build` (twice), `build → web`, plus `src → web`
via `env.GIT_SHA`. All are data edges: order *and* cache inputs.

See also: [Dependencies and outputs](../concepts/dependencies-and-outputs.md),
[Flow schema](flow-schema.md).
