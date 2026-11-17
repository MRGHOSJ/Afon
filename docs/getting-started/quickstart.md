# Quickstart

Two paths: **Path A** runs entirely on your machine in about a minute
(no cluster, no network). **Path B** deploys to a local k3d cluster.

---

## Path A — the engine without a cluster (5 minutes)

The fake driver stores state in a local file. Nothing touches Kubernetes.

### 1. Look at the flow

```bash
cat examples/fake-driver/flow.yaml
```

It declares two nodes: an `image-ref` (produces a ref and a deterministic
digest — it never pulls anything) and a `fake-object` (a ConfigMap holding
that digest).

### 2. Validate

```bash
afon validate -f examples/fake-driver/flow.yaml
```

Parses the file, compiles the graph, checks node configs and `${...}`
references. Touches no driver.

### 3. Plan

```bash
afon plan -f examples/fake-driver/flow.yaml
```

```
plan for "fake-driver" (examples/fake-driver/flow.yaml)
  image          action   will-run
  obj            resource create
summary: 1 will-run, 1 create
```

The banner `fake driver: no cluster is touched` appears on stderr — a
reminder you are on the fake driver.

### 4. Apply

```bash
afon apply -f examples/fake-driver/flow.yaml
```

```
apply result:
  image          succeeded
  obj            created ConfigMap/hello
```

State is written to `examples/fake-driver/.afon/fake-state.json`.

### 5. Plan again

```bash
afon plan -f examples/fake-driver/flow.yaml
```

Now `image` is `cached` and `obj` is `none` — the content-addressed cache
at work. Delete `.afon/` at any time to start fresh; it is safe.

---

## Path B — deploy to a local cluster

### 1. Create a cluster

```bash
k3d cluster create dev
kubectl config current-context    # e.g. k3d-dev — you will pass this below
```

### 2. Pick a real image

The example flows use **placeholder** images so nothing external is pulled
by accident. Edit `examples/hello-app/flow.yaml` and replace:

```yaml
image: registry.example.com/your-org/hello:1.0.0
```

with an image you can pull, for example:

```yaml
image: nginx:1.27-alpine
```

### 3. Render (materialize generated YAML)

```bash
afon render -f examples/hello-app/flow.yaml
```

Writes `examples/hello-app/generated.yaml` with a provenance header.
`afon render --check` exits 1 if the file is stale (CI-friendly).

### 4. Plan against the cluster

```bash
afon plan -f examples/hello-app/flow.yaml --driver k8s --context k3d-dev
```

```
k8s driver: context=k3d-dev namespace=hello-app
plan for "hello-app" (examples/hello-app/flow.yaml)
  app            action   cached
  deploy         resource create
summary: 1 cached, 1 create
```

> If the target namespace does not exist yet, `plan` fails fast with
> `create it before applying` — that is deliberate (plan never creates).
> `apply` will create it for you, or run
> `kubectl create namespace hello-app` first.

### 5. Apply

```bash
afon apply -f examples/hello-app/flow.yaml --driver k8s --context k3d-dev
```

Afon re-plans internally, creates the namespace if needed, applies the
objects with server-side apply, **waits for readiness**, and writes a
release record.

```bash
kubectl get deploy,svc -n hello-app
```

### 6. History, status, rollback

```bash
afon history  -f examples/hello-app/flow.yaml --driver k8s --context k3d-dev
afon status   -f examples/hello-app/flow.yaml --driver k8s --context k3d-dev
afon rollback -f examples/hello-app/flow.yaml --driver k8s --context k3d-dev
```

- `history` lists releases newest first (`v1 succeeded`, …).
- `status` compares live state to the newest record.
- `rollback` replays the previous release's recorded manifests and records
  itself as a new release (append-only history).

---

## What to read next

- [Write a flow](../guides/write-a-flow.md) — authoring your own flow
- [Recipes](../guides/recipes.md) — all five example flows, annotated
- [Concepts: Plans](../concepts/plans.md) — every plan status explained
- [Troubleshooting](../guides/troubleshooting.md) — when something fails
