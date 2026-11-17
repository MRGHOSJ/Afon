# FAQ

### What is Afon, in one sentence?

A local-first CLI that plans and applies declarative flows to Kubernetes
clusters — with content-addressed caching, recorded releases, and
rollback — and grows pipeline nodes (git, image builds, scanners) inside
the same engine.

### Fake driver vs k8s driver — which do I use?

`fake` (the default) runs against a local state file: no cluster, no
network, loud stderr banner. Use it to learn, test, and CI. `k8s` talks
to a real cluster via kubeconfig and is required for release records,
rollback, and `image-build`.

### Why does `plan` fail when the namespace doesn't exist, but `apply` works?

Plan is read-only **by construction** — it creates nothing, so it fails
fast with `create it before applying`. Apply runs its namespace ensure
*before* its internal plan, so a fresh namespace is fine. If you want
`plan` to work first, pre-create the namespace with `kubectl`.

### Where does the cache live, and is it safe to delete?

`<flow-file-dir>/.afon/cache/` (or `--state-dir`). Yes, always safe to
delete — it holds nothing but cached node results. Entries are written
only after a node succeeds, so a crashed run never poisons the cache.

### Why did my image rebuild/re-push when nothing changed?

Almost always: the previous run died **before writing its cache entry**
(killed process, Ctrl-C during the node). No entry → cache miss → rebuild.
With `image-build`, the anonymous registry pre-check then makes this
cheap: if `<image>:<sha>` already carries the `afon.dev/build-config`
label, the Job and push are skipped entirely.

### Where do release records live? Are they safe?

As Secrets named `afon.release.<flow>.v<seq>` in your target namespace.
They contain your manifests and object inventory — treat the namespace
accordingly. Afon reads/writes *only these* Secrets; it never reads
credential Secrets you create (they're referenced by name and mounted
into Jobs).

### Can two flows share a namespace?

They can, but each flow only manages **objects it owns** (owner labels).
An object already owned by another flow is refused, never adopted — so
keep object names distinct per flow, or use separate namespaces (the
simpler habit).

### What is the `afon-build` namespace?

Where `image-build` runs its throwaway kaniko Job and where your
registry/git credential Secrets live (by name). Auto-created with Pod
Security `baseline` labels. Your application namespace never holds build
secrets.

### Why are my example images `registry.example.com/…`?

Examples ship with placeholders so nothing external is pulled or pushed
by accident. Swap them for images you control before applying — see
[Recipes](recipes.md).

### Can I use Helm?

Not yet — Helm rendering is on the [Roadmap](../ROADMAP.md) (Phase 5).
Today: render your chart to YAML and reference it via `k8s-manifests`
paths, or eject manifests manually.

### How do I force a rebuild / fresh run?

```bash
rm -rf <flow-dir>/.afon          # clear caches
rm -f  <flow-dir>/generated.yaml # then afon render again
```

Nothing else needs "resetting" — release history is append-only in the
cluster and never rewritten.

### Does `apply` prompt me?

Only when the plan contains **deletes**. `--auto-approve` skips it
(recommended for CI). No deletes → no prompt, ever.

### What happens if I hit Ctrl-C mid-apply?

In-flight nodes are cancelled, the release record goes terminal `failed`
(never stuck `pending`), stderr prints `error: context canceled`, exit
code is 1, and already-applied objects remain (inventoried). Re-run
when ready.

### Is Afon production-ready?

It is an early learning project — the engine, driver, releases, and
build nodes are real and tested, but expect sharp edges. The honest
status table lives in the repository [README](https://github.com/MRGHOSJ/afon#status).

### How do I upgrade / change a node's behavior without breaking caches?

Node authors bump `TypeVersion()` — every cached entry for that type
invalidates exactly once, then the new results are cached. As a user you
just pull the new binary.

### Where can I see what's planned next?

The [Roadmap](../ROADMAP.md) — phases, "done when" criteria, and what is
explicitly cut. Design rationale lives in [design decisions](../design.md).
