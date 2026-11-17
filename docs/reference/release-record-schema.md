# Release record schema

`afon.release/v1` — the wire format of Afon's release records. Each
record is stored as one Secret in the flow's **target namespace**:

```
name: afon.release.<flow-name>.v<sequence>
type: (opaque; Afon-managed)
```

A backend that cannot parse the `apiVersion` **refuses loudly** —
silently reading a future format is worse than failing.

## Record fields

| Field | Type | Required | Meaning |
|---|---|---|---|
| `apiVersion` | string | yes | Must be `afon.release/v1` |
| `flow` | string | yes | Flow name (`metadata.name`) |
| `sequence` | int ≥ 1 | yes | Release number (`v1`, `v2`, …), assigned by `Start` |
| `status` | enum | yes | `pending` \| `succeeded` \| `failed` \| `rolledback` |
| `trigger` | enum | yes | `manual` \| `rollback` \| `auto` (`auto` reserved; never emitted by v1) |
| `previousSequence` | int | no | Sequence this record succeeded (set by the backend) |
| `rolledBackTo` | int | no | On rollback records: the sequence replayed (must be `1..sequence-1`) |
| `startedAt` | timestamp | yes | When the record was reserved (before any node runs) |
| `finishedAt` | timestamp | yes* | Required for every status **except** `pending` |
| `manifests` | list | no | `{node, yaml}` — each resource node's YAML stream **exactly as applied**. Rollback replays these instead of re-rendering. |
| `inventory` | list | no | Live objects claimed at record time (see below) |
| `error` | string | no | Failure reason (also used for `superseded by --takeover: …`) |

\* Backends fill `finishedAt` at promote time.

### Inventory objects

| Field | Meaning |
|---|---|
| `group`, `version`, `kind`, `namespace`, `name` | Object identity |
| `node` | The resource node whose desired set included it (rollback re-applies under this owner) |
| `uid` | Live UID at record time |
| `hash` | Hex sha256 of the object's canonical live form (volatile keys stripped) — what `status` compares |

## Invariants (validated before every write)

- `apiVersion` matches, `flow` non-empty, `sequence ≥ 1`.
- `status` and `trigger` are known enum values.
- `startedAt` set; `finishedAt` set unless `status == pending`.
- `rolledBackTo`, when set, is a **past** sequence (`1..sequence-1`).
- A `pending` record has no `finishedAt` yet (it isn't final).

## Lifecycle and guards

```
Start() ──► pending ──► succeeded | failed | rolledback
              ▲
              └── concurrency guard: at most one per flow
```

- **`Start` reserves the sequence and writes `pending` before any node
  runs.** A fresh pending record always refuses a second apply:
  ```
  another apply is in progress: release v3 for flow "hello" pending since …
  ```
- **Staleness:** a pending record older than **10 minutes**
  (`PendingStaleAfter`) is considered a crashed run:
  ```
  release v3 for flow "hello" is pending since … (probable crashed run); pass --takeover to override
  ```
  With `--takeover`, the stale record is promoted to `failed` with
  `error: superseded by --takeover: pending since …`, and the new apply
  proceeds. Fresh pending records block even with `--takeover`.
- **Retention:** the newest **10** records per flow are kept; older ones
  are pruned after each promote.
- **Append-only:** nothing is ever edited in place — rollback writes a
  new record; takeover marks the old one failed.

## Status semantics

| Status | Restorable as rollback target? | Inventory |
|---|---|---|
| `succeeded` | yes | full |
| `rolledback` | yes (it applied manifests) | full |
| `failed` | **no** — partial state, manifests success-only | claims live objects it touched (so later prunes can clean up) |
| `pending` | no (not final) | — |

## Who reads/writes records

| Command | Access |
|---|---|
| `apply` | `Start` → (nodes) → promote to terminal |
| `history` | list, newest first (keeps 10) |
| `status` | newest record vs live inventory hashes |
| `rollback` | read target record's manifests/inventory; write new record |
| `plan` | may prune inventory through the backend (owner-scoped) |

Fake driver: no records at all (`rollback` refuses without
`--driver k8s`).

## Related

- [Releases and rollback](../concepts/releases-and-rollback.md) — the
  behavior this schema implements
- [CLI: history/status/rollback](cli.md)
