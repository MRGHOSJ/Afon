# Actions and resources

Every node in a flow is one of exactly two kinds. The kind is registered
in code with the node type — **users never write it**.

| Kind | Contract | What it does | Examples |
|---|---|---|---|
| **Action** | `Run(ctx, cfg, inputs) → outputs` | Runs something and produces outputs other nodes can reference. The **only** place in the engine with side effects. | `image-ref`, `app-manifests`, `k8s-manifests`, `git-source`, `image-build` |
| **Resource** | `Desired(ctx, cfg, inputs) → objects` | Declares objects that should exist. The engine diffs them against live state; the driver applies them. Never "runs". | `k8s-apply`, `fake-object` |

## Why the split matters

**Plans are read-only by construction.** Resource nodes are pure
(describe → diff), and plan never calls `Run` on action nodes — it only
computes cache keys and reads already-materialized outputs. That is why
`afon plan` can promise it changes nothing: there is no code path that
could.

## The full type list today

Registered explicitly in the CLI (no `init()` magic — tests build their
own registries):

| Type | Kind | Produces | Cluster needed? |
|---|---|---|---|
| `image-ref` | action | `ref`, `digest` | no (digest is computed, never pulled) |
| `app-manifests` | action | `rendered` | no |
| `k8s-manifests` | action | `rendered` | no |
| `git-source` | action | `sha`, `url` | no (git network only) |
| `image-build` | action | `digest`, `ref` | **yes** (runs a throwaway Job) |
| `k8s-apply` | resource | — | for apply/plan against k8s |
| `fake-object` | resource | — | no (fake driver only) |

Planned but not built: `trivy-scan`, Helm rendering, notify-webhook,
extension nodes. See the [Roadmap](../ROADMAP.md).

## How nodes connect

Two mechanisms:

1. **References** — `${node.output}` in a config creates a data edge:
   the consumer cannot run until the producer's output exists, and the
   output's value (or its hash) participates in the consumer's cache key.
   See [Dependencies and outputs](dependencies-and-outputs.md).
2. **`needs:`** — an explicit ordering edge without data flow, for when
   you want sequencing only.

The result is a DAG. The executor runs independent nodes concurrently
(default `--parallelism 4`), halts scheduling on first failure, and
honors Ctrl-C cancellation.

## Outputs are typed values

Action outputs are *known values* (a string like a SHA or a rendered YAML
stream). At plan time, a node whose inputs are not yet known reports
`(inputs unknown until "X" runs)` instead of guessing — plan never
executes an action to find out.

## Caching applies to actions only

Resources are diffed every plan (that is cheap — it is a server-side
dry-run). Actions are content-addressed and can be `cached`; see
[Caching](caching.md).

Next: [Plans](plans.md).
