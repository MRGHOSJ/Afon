# Node authoring

How to implement a new node type. **Advanced / extension authors** —
everyday flow writing needs none of this; start with
[Write a flow](../guides/write-a-flow.md).

## The contract

A node type is a Go type implementing `engine.NodeType`:

```go
type NodeType interface {
    Type() string            // the name users write in flow.yaml
    TypeVersion() string     // cache-busting version (see below)
    Kind() Kind              // KindAction or KindKindResource — registered in code
    Validate(cfg any) error  // structural check at compile time
    DeclaredOutputs() []string
}
```

Plus **exactly one** kind-specific interface:

| Kind | Interface | Signature |
|---|---|---|
| Action | `ActionType` | `Run(ctx, cfg any, in Inputs) (Outputs, error)` |
| Resource | `ResourceType` | `Desired(ctx, cfg any, in Inputs) ([]driver.Object, error)` |

The kind belongs to the type — **users never write it** in the flow file.

## Registration is explicit (no `init()`)

```go
// cmd/afon/flow.go
for _, nt := range []engine.NodeType{
    engine.ImageRef{},
    // …
    build.GitSource{},
    mypkg.MyNode{},        // ← add yours here
} {
    if err := reg.Register(nt); err != nil { return nil, err }
}
```

Duplicate `Type()` names are an error. Tests build their own registries
(`engine.NewRegistry()` + `Register`) — that's why there's no global
init: isolation by construction.

## Validation rules

`Validate(cfg any)` receives the config as `map[string]any` (references
still present as `engine.Reference` / `engine.Template` markers — accept
them wherever a resolved string will arrive later):

- Reject unknown fields (typos must fail at compile time).
- Check required fields, types, ranges, grammars (e.g. DNS-1123).
- Accept markers: a literal gets full checks now; a marker gets the same
  checks again at run time in your config resolver.
- Return plain errors — the engine prefixes position context.

## Declared outputs

`DeclaredOutputs()` is the compile-time contract for `${node.output}`:
Compile rejects references to names you didn't declare, and the engine
sets your `Run` results into that map. Keep it minimal and stable.

## Optional extensions

| Interface | When to implement |
|---|---|
| `InputCacher` | Your output depends on state **outside the config** (a moved git ref, a file's contents). Return a canonical string; the engine composes `{config, extra}` into the key. Errors are asymmetric **by design**: plan degrades to `will-run` with the reason; apply fails the node. |
| `ClusterAware` | You need a cluster to Run (throwaway Job). Keep `Run` as the no-cluster entry point — it must fail clearly (`requires the k8s driver (apply only)`), never panic. Implement `RunCluster(ctx, cfg, in, cluster any)`; type-assert the payload defensively (plan attaches it only for k8s-driver apply). A **verified cache hit never touches the cluster.** |
| `MaterializedNode` | Your output should live in a file plan/apply read back. Return a literal path from `MaterializedPath`; the engine handles the provenance header, hand-edit protection, and `render --check`. Declare exactly one output. |

## TypeVersion discipline

`TypeVersion()` is an input to every cache key for your type:

- Bump it when **Run's output shape or behavior changes**.
- Effect: all cached entries for the type invalidate **once**, then the
  new results cache normally.
- Don't bump for refactors that can't change outputs — cache thrash is
  user-visible.

Precedent: `image-build` went `1 → 2` when the registry pre-check +
`--label` landed; users' next apply rebuilt once, then the registry
itself carried the proof.

## Testing pattern

Mirror the existing tests (`pkg/build/*_test.go`, `pkg/engine/*_test.go`):

1. **Shape test** — `Type()`, `TypeVersion()`, `Kind()`, outputs.
2. **Validate table test** — valid config accepted; each bad field
   (missing, wrong type, unknown key, out-of-range) rejected with the
   right message.
3. **Run test** — with `Inputs.Upstream` seeded; assert outputs.
4. **Cache test** — same inputs → same key; changed external input →
   different key (`InputCacher`).
5. **Cluster test** if `ClusterAware` — no-cluster `Run` fails clearly;
   `RunCluster` with a stub payload; type-assertion failure is an error,
   not a panic.
6. **Flow e2e** (optional) — register your type in a test registry,
   compile a flow, plan/apply through the fake driver.

## Dependency rule

The engine never imports Kubernetes types. Your node lives in a package
**outside** `pkg/engine` if it needs cluster types (see `pkg/build`:
`cmd → build → engine`). The cluster payload stays `any` at the engine
layer.

## Checklist

- [ ] `Type()` name is unique and kebab-case (users type it)
- [ ] `Kind()` matches the single interface you implement
- [ ] `Validate` rejects unknown fields and accepts ref markers
- [ ] `DeclaredOutputs()` matches what `Run` returns
- [ ] `TypeVersion()` bumped if behavior changed
- [ ] Side effects only in `Run`/`RunCluster`/`Desired` (plan stays read-only)
- [ ] Table tests for Validate + Run + cache
- [ ] Registered explicitly in `cmd/afon/flow.go`
