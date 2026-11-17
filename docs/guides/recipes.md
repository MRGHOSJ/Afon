# Recipes

Five copy-paste flows covering every registered node type. All external
references are **placeholders** — swap `your-org`, `registry.example.com`,
`your-repo` for your own. The same flows live under
[`examples/`](https://github.com/MRGHOSJ/afon/tree/main/examples) in the
repository.

Placeholder conventions used throughout:

- **Repository:** `https://github.com/your-org/your-repo.git`
- **Image:** `registry.example.com/your-org/<app>` (tagless for
  `image-build` — the tag is always the resolved git SHA)
- **Registry Secret:** `registry-creds`
  (`kubernetes.io/dockerconfigjson` in the `afon-build` namespace)

---

## 1. Fake driver — no cluster at all

**Demonstrates:** `image-ref` → `fake-object`, content-addressed caching,
the validate/plan/apply loop. Runs anywhere, offline.

```yaml
apiVersion: afon.dev/flow/v1
kind: Flow
metadata:
  name: fake-driver
spec:
  nodes:
    - id: image
      type: image-ref
      config:
        ref: registry.example.com/your-org/app:1.0.0  # never pulled; digest is computed
    - id: obj
      type: fake-object
      config:
        kind: ConfigMap
        name: app-digest
        data:
          digest: ${image.digest}
```

```bash
afon validate -f examples/fake-driver/flow.yaml
afon plan     -f examples/fake-driver/flow.yaml    # will-run + create
afon apply    -f examples/fake-driver/flow.yaml    # writes .afon/fake-state.json
afon plan     -f examples/fake-driver/flow.yaml    # cached + none
```

---

## 2. Quick-start app spec

**Demonstrates:** `app-manifests` (generate a Deployment + Service from a
6-field spec) → `k8s-apply`, plus `render` materialization.

```yaml
apiVersion: afon.dev/flow/v1
kind: Flow
metadata:
  name: hello-app
spec:
  target:
    namespace: hello-app
  nodes:
    - id: app
      type: app-manifests
      config:
        name: hello
        image: registry.example.com/your-org/hello:1.0.0  # placeholder — use e.g. nginx:1.27-alpine
        port: 8080
        out: generated.yaml
    - id: deploy
      type: k8s-apply
      config:
        manifests: ${app.rendered}
```

```bash
afon render -f examples/hello-app/flow.yaml        # writes generated.yaml
afon plan   -f examples/hello-app/flow.yaml --driver k8s --context k3d-dev
afon apply  -f examples/hello-app/flow.yaml --driver k8s --context k3d-dev
kubectl get deploy,svc -n hello-app
```

---

## 3. Raw Kubernetes manifests

**Demonstrates:** `k8s-manifests` (globs over hand-written YAML, target
namespace injection) → `k8s-apply`.

`examples/manifests/flow.yaml`:

```yaml
apiVersion: afon.dev/flow/v1
kind: Flow
metadata:
  name: manifests-demo
spec:
  target:
    namespace: manifests-demo
  nodes:
    - id: manifests
      type: k8s-manifests
      config:
        paths:
          - deploy/*.yaml
    - id: apply
      type: k8s-apply
      config:
        manifests: ${manifests.rendered}
```

`examples/manifests/deploy/app.yaml`:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
spec:
  replicas: 1
  selector:
    matchLabels:
      app: app
  template:
    metadata:
      labels:
        app: app
    spec:
      containers:
        - name: app
          image: registry.example.com/your-org/app:1.0.0  # placeholder
          ports:
            - containerPort: 8080
---
apiVersion: v1
kind: Service
metadata:
  name: app
spec:
  selector:
    app: app
  ports:
    - port: 80
      targetPort: 8080
```

```bash
afon plan  -f examples/manifests/flow.yaml --driver k8s --context k3d-dev
afon apply -f examples/manifests/flow.yaml --driver k8s --context k3d-dev
```

---

## 4. Image substitution (digest pinning mechanics)

**Demonstrates:** `image-ref` → `k8s-manifests`' `images:` mapping
swapping a container image at render time → `k8s-apply`.

`examples/pinned-image/flow.yaml`:

```yaml
apiVersion: afon.dev/flow/v1
kind: Flow
metadata:
  name: pinned-image-demo
spec:
  target:
    namespace: pinned-demo
  nodes:
    - id: image
      type: image-ref
      config:
        ref: registry.example.com/your-org/app:1.0.0  # computed digest; never pulled
    - id: manifests
      type: k8s-manifests
      config:
        paths:
          - deploy/*.yaml
        images:
          app: ${image.ref}  # overwrites the container named "app"
    - id: apply
      type: k8s-apply
      config:
        manifests: ${manifests.rendered}
```

The manifest's container image (`registry.example.com/your-org/app:0.0.0`)
is a placeholder that the `images:` mapping replaces — match the **container
name** (`app`), not the image string.

```bash
afon plan  -f examples/pinned-image/flow.yaml --driver k8s --context k3d-dev
afon apply -f examples/pinned-image/flow.yaml --driver k8s --context k3d-dev
```

---

## 5. Full pipeline: git → build → deploy

**Demonstrates:** `git-source` → `image-build` (throwaway kaniko Job,
digest-pinned push) → `app-manifests` → `k8s-apply`. The flagship flow.

```yaml
apiVersion: afon.dev/flow/v1
kind: Flow
metadata:
  name: git-build-demo
spec:
  target:
    namespace: git-build-demo
  nodes:
    - id: src
      type: git-source
      config:
        url: https://github.com/your-org/your-repo.git  # placeholder
        ref: main
    - id: build
      type: image-build
      config:
        image: registry.example.com/your-org/your-app  # placeholder; no tag (by design)
        repo: ${src.url}
        sha: ${src.sha}
        registrySecret: registry-creds  # Secret name in afon-build
    - id: web
      type: app-manifests
      config:
        name: your-app
        image: ${build.ref}  # digest-pinned reference from the build
        port: 8080
    - id: deploy
      type: k8s-apply
      config:
        manifests: ${web.rendered}
```

Before applying:

1. Replace the repo URL and tagless image path.
2. Create the registry Secret in the build namespace:
   ```bash
   kubectl create secret generic registry-creds \
     --type=kubernetes.io/dockerconfigjson \
     --from-file=.dockerconfigjson=$HOME/.docker/config.json \
     -n afon-build
   ```
   (the `afon-build` namespace is created automatically with baseline
   labels on first build).

```bash
afon plan  -f examples/git-build-deploy/flow.yaml --driver k8s --context k3d-dev
afon apply -f examples/git-build-deploy/flow.yaml --driver k8s --context k3d-dev
```

What happens: git ref resolved → Job in `afon-build` checks out the repo
and runs pinned kaniko → pushes `<image>:<git-sha>` **with the
`afon.dev/build-config` label** → digest-pinned `ref` flows into the
Deployment. On later runs with a cleared local cache, the anonymous
**registry pre-check** sees the label and skips the Job entirely.

---

## Drills

### Drill A — break it, watch it roll back

```bash
# add onFailure: rollback to the deploy node, then apply a deliberately
# broken manifest (e.g. readiness probe pointing at a closed port).
afon apply -f flow.yaml --driver k8s --context k3d-dev
# → node fails its readiness wait, afon prints "rolling back to release N"
afon history -f flow.yaml --driver k8s --context k3d-dev
# → v1 succeeded, v2 failed, v3 rolledback
```

### Drill B — cache vs registry skip

```bash
afon apply -f examples/git-build-deploy/flow.yaml --driver k8s --context k3d-dev   # real build (~minutes)
rm -rf examples/git-build-deploy/.afon/cache
afon apply -f examples/git-build-deploy/flow.yaml --driver k8s --context k3d-dev   # registry pre-check: no Job, seconds
```

Compare `kubectl get jobs -n afon-build` timestamps between the two runs.

### Drill C — Ctrl-C contract

```bash
afon apply -f flow.yaml --driver k8s --context k3d-dev
# press Ctrl-C while a node runs
# → in-flight nodes cancelled (not failed), record goes terminal failed,
#   stderr shows "error: context canceled", exit code 1
```

Next: [Local development](local-development.md).
