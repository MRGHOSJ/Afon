# Troubleshooting

Symptom → cause → fix. Messages are quoted as Afon (and Kubernetes)
actually print them.

## Plan and validation

### `create it before applying`

**Where:** `afon plan --driver k8s` against a namespace that doesn't
exist yet.
**Why:** plan never creates anything (read-only contract); it fails fast
instead of planning into a void.
**Fix:** either run `afon apply` (it auto-creates the target namespace
before its internal plan), or pre-create it:

```bash
kubectl create namespace <name>
```

`validate`, `status`, and `history` never hit this (they don't diff
cluster state against a namespace they'd need to create).

### `unknown field "path"` / `missing required field "url"`

**Where:** `afon validate` (or plan).
**Why:** node configs are strict — unknown fields are refused so typos
fail at compile time, not at apply.
**Fix:** check the config table for that type in
[Node types](../reference/node-types.md).

### `invalid reference ${a.b.c}: want ${node.output}, ${secret.NAME}, or ${env.NAME}`

**Why:** regular references have exactly one dot; `${secret.x.y}` names
may not contain dots either.
**Fix:** one dot, non-empty sides. Literal `${` in a value → `$${`.

### `unknown output "shas" of node "build"`

**Why:** the producer doesn't declare that output.
**Fix:** use declared outputs — e.g. `image-build` has `digest` and
`ref` (not `sha`); `git-source` has `sha` and `url`.

### `dependency cycle: deploy → build → deploy`

**Why:** refs/`needs` form a loop.
**Fix:** break the cycle — typically a config accidentally referencing a
downstream node.

## Cluster connectivity

### `cannot reach cluster "k3d-dev" (https://127.0.0.1:53502): connection refused — is the cluster running?`

**Why:** the kubeconfig context points at a dead API server (stopped
cluster, recreated container, reassigned port).
**Fix:**

```bash
kubectl cluster-info          # does kubectl agree it's down?
k3d cluster start dev         # or recreate the cluster
kubectl config view --minify  # confirm server URL/port matches reality
```

Afon prints the exact server URL it tried — compare it with
`kubectl config view --minify`.

## Apply failures

### `FailedMount … secret "missing-creds" not found`

**Where:** a build Job pod, surfaced in the apply error.
**Why:** `registrySecret`/`gitSecret` names a Secret that doesn't exist
**in the build namespace** (`afon-build` by default).
**Fix:**

```bash
kubectl create secret generic registry-creds \
  --type=kubernetes.io/dockerconfigjson \
  --from-file=.dockerconfigjson=$HOME/.docker/config.json \
  -n afon-build
```

Afon surfaces pod events instead of reading Secrets — it will never tell
you the *contents*, only that the mount failed.

### `build namespace "afon-build" does not exist and afon cannot create it: … (create it manually: kubectl create namespace afon-build)`

**Why:** your identity lacks `create` on namespaces (or admission
denied it).
**Fix:** run the hinted command yourself, or grant the permission.

### PodSecurity `… violates PodSecurity "baseline": …`

**Why:** something in the build pod spec is above baseline (normally a
misconfigured cluster policy — Afon's own spec is baseline-compatible:
RuntimeDefault seccomp, no privilege).
**Fix:** check the namespace's
`pod-security.kubernetes.io/enforce` label; Afon's build namespace is
created with `baseline`. Afon never relabels namespaces itself.

### `image must not include a tag`

**Why:** `image-build` is tagless **by design** — the tag is always the
resolved git SHA, so `:latest` is impossible.
**Fix:** `image: registry.example.com/your-org/app` (no `:tag`).

### `image "…" not found` / `ImagePullBackOff`

**Why:** you left a placeholder image in the flow (examples ship
`registry.example.com/...` on purpose so nothing external is pulled by
accident).
**Fix:** swap in an image your cluster can pull (e.g. `nginx:1.27-alpine`).

### `fields owned by …; re-run with --force-conflicts to take ownership`

**Why:** server-side apply found fields managed by another field manager
(a previous manual `kubectl apply`, another tool).
**Fix:** decide who owns the object. If it's yours now:

```bash
afon apply -f flow.yaml --driver k8s --context <ctx> --force-conflicts
```

### `Deployment/foo is owned by flow "other" node "deploy"; refusing to manage it from another owner`

**Why:** the live object carries another flow's owner labels. Afon never
adopts foreign objects.
**Fix:** remove it from *that* flow, delete it manually, or rename yours
— but don't fight the guard.

### `another apply is in progress: release v3 for flow "hello" pending since …`

**Why:** a previous apply is (or was) still marked `pending` and is
**fresh** (< 10 minutes).
**Fix:** wait for it. Fresh pending records always block — even
`--takeover` won't cross that window.

### `release v3 for flow "hello" is pending since … (probable crashed run); pass --takeover to override`

**Why:** a previous apply crashed (killed process, slept laptop) and the
record has been `pending` for > 10 minutes.
**Fix:**

```bash
afon apply -f flow.yaml --driver k8s --context <ctx> --takeover
```

The stale record is promoted to `failed` (marked
`superseded by --takeover`) and the run proceeds.

## Cancellation (Ctrl-C)

**What you see:** `error: context canceled` on stderr, **exit code 1**,
in-flight nodes show `cancelled` (not `failed`), the release record ends
terminal `failed`.

**Is that okay?** Yes — by contract. Records never stay stuck `pending`
after a clean Ctrl-C; objects the run already applied stay in the
cluster, inventoried under the failed record so a later apply/rollback
can prune them. Re-run when ready.

## Render

### `… exists without a valid render header; refusing to overwrite without --force`

**Why:** `afon render` won't clobber a file it didn't generate.
**Fix:** delete the file, or `afon render --force`.

### `… has been edited since it was rendered; refusing to clobber hand-edits — pass --force to overwrite`

**Why:** your edits to `generated.yaml` would be lost — Afon protects
them.
**Fix:** move the change into the flow (the source of truth), or take
ownership with `--force`.

### `afon render --check` exits 1

**Meaning:** the materialized file is stale (flow changed since last
render). Intended for CI: run `afon render` and fail if the check fails.

## Rollback

### `afon rollback: release records require the k8s driver`

**Why:** rollback reads/writes release Secrets — fake driver has none.
**Fix:** add `--driver k8s --context <ctx>`.

### Rollback replayed but something is still wrong

**Expected limits:** rollback replays **recorded manifests** — it cannot
undo database migrations, volume contents, CRD schema changes, or other
out-of-cluster effects. See [Releases and rollback](../concepts/releases-and-rollback.md).

## Still stuck

1. `afon validate -f flow.yaml` — position-accurate errors first.
2. `afon plan -o json -f flow.yaml` — structured view of what's known.
3. `afon inspect --context <ctx>` — is the cluster reachable and healthy?
4. `kubectl get events -n <ns> --sort-by=.lastTimestamp` — raw cluster
   truth (Afon surfaces pod events in errors for exactly this reason).
5. Delete `.afon/` — eliminate stale-cache theories in one command.

Next: [FAQ](faq.md).
