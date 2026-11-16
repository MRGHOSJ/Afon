# Flow examples

Five flows covering every registered node type. All external references are
**placeholders** — swap `your-org`, `registry.example.com`, and
`your-repo` for your own before applying. Nothing here pulls a real image
or clones a real repository as written.

| Example | Kind | Nodes | Cluster |
|---|---|---|---|
| [`fake-driver`](fake-driver) | Offline engine demo | `image-ref` → `fake-object` | no (default fake driver) |
| [`hello-app`](hello-app) | Quick-start app spec | `app-manifests` → `k8s-apply` | yes |
| [`manifests`](manifests) | Raw Kubernetes YAML | `k8s-manifests` → `k8s-apply` | yes |
| [`pinned-image`](pinned-image) | Image substitution | `image-ref` → `k8s-manifests` → `k8s-apply` | yes |
| [`git-build-deploy`](git-build-deploy) | Full git → build → deploy pipeline | `git-source` → `image-build` → `app-manifests` → `k8s-apply` | yes |

## Try one

```sh
# No cluster needed:
afon plan -f examples/fake-driver/flow.yaml

# Against a local k3d / kind / k3s cluster (apply auto-creates the
# target namespace; plan against a fresh namespace needs it to exist):
afon render -f examples/hello-app/flow.yaml
afon plan   -f examples/manifests/flow.yaml --driver k8s --context k3d-dev
afon apply  -f examples/manifests/flow.yaml --driver k8s --context k3d-dev

# The full pipeline additionally needs a registry Secret in afon-build
# (image-build mounts it by name; see the header comment in the flow):
afon apply -f examples/git-build-deploy/flow.yaml --driver k8s --context k3d-dev
```

## Placeholder conventions

- **Repository:** `https://github.com/your-org/your-repo.git`
- **Image:** `registry.example.com/your-org/<app>` — `image-build` takes a
  tagless path by design (the tag is always the resolved git sha)
- **Registry Secret:** `registry-creds` (`kubernetes.io/dockerconfigjson`,
  lives in the `afon-build` namespace, referenced by name only)
- `image-ref` never contacts a registry: it computes a deterministic
  digest from the ref string so substitution and caching work offline
