# Local development

Everything you need to develop and demo Afon on one machine: a local
cluster, the fake driver, the bundled demo flows, and cache hygiene.

## k3d in two minutes

```bash
# install: https://k3d.io
k3d cluster create dev
kubectl config current-context        # e.g. k3d-dev
afon inspect --context k3d-dev        # read-only probe — proves connectivity
k3d cluster delete dev                # when you're done
```

Notes:

- k3d stores state in Docker; `k3d cluster delete` is fully clean.
- If your API port changes (Docker restarts can reassign it), Afon reads
  the kubeconfig — if kubectl works, Afon works. A connection refusal
  prints `cannot reach cluster "…" (…): connection refused — is the
  cluster running?`.
- kind / k3s work the same way; only the context name differs.

## The fake driver (default)

Every flow command defaults to `--driver fake`:

```bash
afon plan  -f flow.yaml        # no cluster, banner: "fake driver: no cluster is touched"
afon apply -f flow.yaml        # state → <flow-dir>/.afon/fake-state.json
```

Use it for:

- Learning the engine with zero setup ([Quickstart Path A](../getting-started/quickstart.md)).
- CI tests that must not touch a cluster.
- Validating graph/cache behavior while iterating on a flow.

It loudly banners on stderr so a fake apply can never be mistaken for a
real one (and `-o json` stdout stays parseable).

## State and cache hygiene

| Task | Command |
|---|---|
| Clear caches | `rm -rf <flow-dir>/.afon` (safe — pure cache) |
| Relocate state | `--state-dir /path` (default: the flow file's directory, never cwd) |
| Fresh fake cluster | delete `.afon/fake-state.json` |
| Re-materialize outputs | `afon render -f flow.yaml --force` (after hand-edits) |

The cache writes entries **only after a node succeeds** — a killed run
leaves no stale entries behind, so "clear the cache" is never required
for correctness, only to force redo.

## The bundled demo

The repository ships a worked demo (not part of the docs site):

```
demo/
├── flow.yaml                  # minimal k8s-manifests → k8s-apply
└── transit-backend/
    └── flow.yaml              # full git-source → image-build → deploy pipeline
```

Typical session against a local cluster:

```bash
afon apply  -f demo/flow.yaml --driver k8s --context k3d-dev
afon plan   -f demo/flow.yaml --driver k8s --context k3d-dev   # cached / none
afon apply  -f demo/transit-backend/flow.yaml --driver k8s --context k3d-dev
afon history -f demo/transit-backend/flow.yaml --driver k8s --context k3d-dev
afon status  -f demo/transit-backend/flow.yaml --driver k8s --context k3d-dev
```

For the pipeline demo you need a registry Secret in `afon-build` (see
[Recipe 5](recipes.md#5-full-pipeline-git--build--deploy)).

## Suggested local loop when changing Afon itself

```bash
gofmt -l .
go vet ./...
go build ./...
go test ./... -race -count=1 -timeout 300s
go build -o afon.exe ./cmd/afon     # rebuild the CLI, then try it on demo/
```

(That is the project's own gate — see the repository
[Roadmap](../ROADMAP.md) rules.)

## Ports and firewalls

- k3d API server: `6443` mapped to a host port (shown in kubeconfig).
- Registry pushes happen **from inside the cluster** (kaniko), so your
  machine only needs git + kubeconfig access; no local Docker daemon
  involvement in builds.

## Two clusters, one kubeconfig

```bash
kubectl config get-contexts          # list
afon inspect --context k3d-dev       # pick explicitly, always
afon plan -f flow.yaml --driver k8s --context k3d-dev
```

Afon never guesses: the plan header prints the context and namespace it
is about to touch, before anything runs. You can also pin it in the flow
via `spec.target.context` (the `--context` flag wins).

Next: [Troubleshooting](troubleshooting.md).
