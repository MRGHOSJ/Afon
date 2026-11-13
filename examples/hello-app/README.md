# hello-app

Deploys a Deployment running `nginx:1.27-alpine` on port 8080, plus a ClusterIP Service on port 80 → 8080, from a 6-field app spec.

`afon render` writes `generated.yaml` (the gitignored artifact, with a provenance header); `afon plan` and `afon apply` read it as-is and never render implicitly.

To mix hand-written YAML with the generated file, add a `k8s-manifests` node with `paths: [generated.yaml, extra.yaml]` and feed its output to `k8s-apply` instead.

```sh
kubectl create namespace hello-app
afon render -f examples/hello-app/flow.yaml
afon apply --driver k8s --context <yours> -f examples/hello-app/flow.yaml   # or afon plan ... for a dry-run first
kubectl get deploy,svc -n hello-app
```
