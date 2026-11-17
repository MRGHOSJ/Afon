# Releases and rollback

Every successful-or-not `apply` with the k8s driver writes a **release
record**: a numbered, append-only account of what went out. Records live
in the cluster as Secrets in the target namespace:

```
afon.release.<flow-name>.v<sequence>
```

## Record lifecycle

```
                ┌──────────────┐
   Start() ───► │   pending    │  written BEFORE any node runs
                └──────┬───────┘         (concurrency guard)
                       │
        ┌──────────────┼───────────────┐
        ▼              ▼               ▼
  ┌───────────┐  ┌──────────┐   ┌────────────┐
  │ succeeded │  │  failed  │   │ rolledback │
  └───────────┘  └──────────┘   └────────────┘
```

| Status | Meaning |
|---|---|
| `pending` | The apply is (or was) in flight. At most **one per flow** — it is the concurrency guard. |
| `succeeded` | Every node finished; the record holds the applied manifests + inventory. |
| `failed` | The run failed or was cancelled (Ctrl-C). Partial state is *inventoried* (live objects it touched are claimed so a later rollback can prune them); manifests stay success-only. |
| `rolledback` | A rollback replay completed; `rolledBackTo` names the sequence it replayed. |

Triggers: `manual` (explicit apply), `rollback` (explicit or automatic),
`auto` (reserved for future automation).

**Append-only:** a rollback never edits history — it writes a *new*
record naming what it replayed. The newest 10 records per flow are kept.

## Pending = crash detection and concurrency

Because `pending` is written before the first node runs:

- A second `apply` while one is in flight is **refused**:
  ```
  another apply is in progress: release v3 for flow "hello" pending since 2026-11-16 09:00:00
  ```
- If a previous run **crashed** (laptop slept, process killed), the
  record stays `pending`. After **10 minutes** it is considered stale:
  ```
  release v3 for flow "hello" is pending since ... (probable crashed run); pass --takeover to override
  ```
  `--takeover` promotes the stale record to `failed` (marked
  `superseded by --takeover`) and proceeds. **Fresh** pending records
  always block — `--takeover` only crosses the 10-minute window.

## `history`

```bash
afon history -f flow.yaml --driver k8s --context <ctx>
```

```
SEQ  STATUS     TRIGGER  AGE  OBJECTS
7    succeeded  manual   23h  2
6    failed     manual   23h  0
5    rolledback rollback 2d   1
```

Newest first. `OBJECTS` is the inventory size. `-o json` for scripting.

## `status`

```bash
afon status -f flow.yaml --driver k8s --context <ctx>
```

Compares **live cluster state** against the newest record's inventory
(content hashes of canonical live objects). Output is deliberately terse:

```
release v7: succeeded (manual, started 23h ago)
  ok 2
```

Exit code 0 when live matches the record, non-zero on divergence — CI
friendly. Note: the comparison anchors on the newest record; a flow whose
newest record is `failed` shows that record, not the last success.

## `rollback`

```bash
afon rollback -f flow.yaml --driver k8s --context <ctx>          # replay v(n-1)
afon rollback -f flow.yaml --driver k8s --context <ctx> --to 5   # replay v5
```

- Replays the **recorded manifests exactly as they were applied** — it
  does not re-render the flow (the flow may have changed; the record is
  ground truth).
- Prunes objects the target release does not own (owner labels).
- Writes a **new** record (`trigger: rollback`, `rolledBackTo: N`).
- Requires `--driver k8s` (fake driver has no records).
- Same wait/confirm/takeover flags as `apply`.
- **Limits:** rollback cannot undo database migrations, volume contents,
  CRD schema changes, or anything outside the cluster.

## Automatic rollback: `onFailure: rollback`

```yaml
- id: deploy
  type: k8s-apply
  config:
    manifests: ${web.rendered}
  onFailure: rollback
```

When this node fails (including a failed readiness wait), the engine
automatically rolls back to the previous release and records it — the run
prints `rolling back to release N`. The default behavior without the
field is `halt`: stop scheduling, record `failed`, leave state as-is for
you to inspect.

## What failed records are for

A `failed` record inventories the live objects that run touched. That
inventory lets a later `rollback` (or a later apply's prune) clean up
partial state — nothing is orphaned just because the run died halfway.

Next: [Cluster profile](cluster-profile.md).
