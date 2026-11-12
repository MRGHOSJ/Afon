# hello-app

`afon render` writes `generated.yaml` (the gitignored artifact, with a provenance header); `afon plan` and `afon apply` read it as-is and never render implicitly.

```sh
kubectl create namespace hello-app
afon render -f examples/hello-app/flow.yaml
afon apply --driver k8s --context <yours> -f examples/hello-app/flow.yaml   # or afon plan ... for a dry-run first
```
