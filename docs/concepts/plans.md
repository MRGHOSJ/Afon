# Plans

`afon plan` answers one question: **what would `apply` do?** — without
executing anything.

## Plan statuses

### Action nodes

| Status | Meaning |
|---|---|
| `will-run` | Cache miss — the node will execute on apply. |
| `cached` | Cache hit — inputs unchanged; apply will skip it. |
| `will-run (inputs unknown until "src" runs)` | The node depends on an action that hasn't been resolved yet (plan doesn't execute actions, so downstream cache keys can't be computed until apply runs them). |

### Resource nodes

| Status | Meaning |
|---|---|
| `create` | The object doesn't exist live. |
| `update` | It exists and the desired form differs. |
| `none` | Live already matches desired. |
| `known-after-apply` | The desired form can't be computed yet (depends on an unresolved action output, e.g. a digest that `image-build` will produce). |

Example:

```
plan for "transit-demo" (demo/transit-backend/flow.yaml)
  src              action   will-run           key=10ec92f0cfdc
  build            action   will-run           (inputs unknown until "src" runs)
  web              action   will-run           (inputs unknown until "build" runs)
  deploy           resource known-after-apply  (config unknown until "web" runs)
summary: 3 will-run, 1 known-after-apply
```

## The plan header

```
driver: k8s context: k3d-dev namespace: afon-demo force: off prune: on
```

- **driver** comes from `--driver` (never from the flow file).
- **context / namespace** — where the plan would run.
- **force** — `--force-conflicts` (take field ownership on SSA conflicts).
- **prune** — on by default; `--no-prune` disables deleting objects that
  were removed from the flow.

## What plan does and does not do

| Does | Does not |
|---|---|
| Parse + compile the flow (`validate` is a subset) | Execute any action node |
| Compute cache keys (may call `git ls-remote` for `git-source`) | Create namespaces, objects, or records |
| Server-side dry-run against the cluster (k8s driver) | Write to `.afon/cache` — **wait**: plan *reads* cache; a cache miss at plan does not execute |
| Diff owned objects, list prune candidates | Prompt for anything (prompts are apply-only) |

**Plan is side-effect-free by construction**: only action `Run` has side
effects, and plan never calls it. The one network activity is cache-key
input resolution (`git ls-remote`), which is a read.

## Network failures degrade, they don't fail

`git-source` resolves refs at plan time. If the host is unreachable, plan
reports the node as `will-run` **with the reason** instead of erroring —
plan must stay usable offline. At apply time the same failure *does* fail
the node (execution needs the answer). This asymmetry is deliberate.

## Plan vs apply's internal plan

`apply` re-plans before executing — a plan is a preview, not a contract.
Consequences worth knowing:

- Things may differ between your `plan` and the `apply` that follows
  (someone else changed the cluster; a ref moved).
- `plan` against a **missing namespace** fails fast with
  `create it before applying` — plan never creates. `apply` **does**
  create the target namespace first (it runs `ensure` before its internal
  plan), so `apply` works on a fresh namespace while `plan` doesn't.
  See [Security model](security-model.md).

## Confirmation

When a plan contains **deletes** (pruned objects or replaced resources),
`apply` asks for confirmation first:

```
Plan contains 2 delete(s). Apply? [y/N]:
```

`--auto-approve` skips the prompt (CI). A plan with no deletes never
prompts.

## Machine-readable plans

```bash
afon plan -f flow.yaml -o json
```

Emits structured entries (id, status, reason, diff summary) on stdout;
banners go to stderr so the JSON stays parseable.

Next: [Caching](caching.md).
