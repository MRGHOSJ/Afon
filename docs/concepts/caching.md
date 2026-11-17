# Caching

Afon's cache is **content-addressed**: a node's result is keyed by its
inputs, not by time or by "did someone run this before". Same inputs →
same key → skip. Different inputs → new key → run.

## The cache key

Roughly:

```
key = sha256(canonical(node config) + extra external inputs + type version)
```

1. **Canonical config** — the node's resolved configuration, serialized
   deterministically (sorted keys), so YAML formatting never changes a
   key.
2. **External inputs** — state the config cannot express, contributed by
   the node through the optional `InputCacher` extension:
   - `git-source` contributes the **resolved SHA**. A moved branch →
     different key → downstream `image-build` rebuilds automatically.
3. **Type version** — every node type declares `TypeVersion()`. Bump it
   when the node's behavior or output shape changes, and every cached
   entry for that type invalidates once. (Example: `image-build` went
   `1 → 2` when the registry pre-check landed — old caches rebuilt once,
   then the registry itself carried the proof.)

The short hex you see in plan output (`key=10ec92f0cfdc`) is the cache
key prefix.

## Where entries live

```
<state-dir>/.afon/cache/<64-hex>.json
```

- `<state-dir>` = `--state-dir` if given, else the flow file's directory
  (**never** the process cwd).
- Writes are atomic (temp file + rename).
- Entries are written **only after the node succeeds**.

That last rule matters: a run that crashes or is Ctrl-C'd mid-node leaves
**no** entry, so the next run redoes that node. This is by design — a
half-finished result must never be replayed as a hit. (It also explains
the classic "why did it rebuild/re-push?" mystery: the previous run died
before writing its entry.)

**Deleting `.afon/` is always safe.** It is pure cache.

## `cached` vs registry-level skip (`image-build`)

`image-build` has *two* independent skip doors:

1. **Local cache hit** — nothing about inputs changed since the last
   successful build. Plan shows `cached`; the Job never starts.
2. **Registry pre-check** (on a local cache *miss*) — before starting a
   Job, Afon asks the registry **anonymously** (no credentials, no Secret
   reads): does `<image>:<git-sha>` already exist and does its config
   carry this build's label `afon.dev/build-config=<content-key>`?
   - **Match** → the image was already built from identical inputs
     (e.g. someone deleted `.afon/`, or another machine built it). The
     node succeeds **without a Job and without a push**.
   - **Any doubt** — image missing, no label, index manifest, Basic-auth
     wall, network error, body too large → **build**. Doubt always falls
     through to a real build; the pre-check can only *save* work, never
     skip a needed build.

The label is applied by kaniko via `--label` during the push, so the
registry becomes a shared, durable build cache — the local `.afon/` is
just an optimization on top.

## What is NOT cached

- **Resources** (`k8s-apply`, `fake-object`) — always diffed (a
  server-side dry-run is cheap and live state changes independently).
- **Release records** — every apply writes one, unconditionally.
- **Plan results** — plans are recomputed every time (live state may have
  changed).

## Invalidation cheat-sheet

| You changed… | Effect |
|---|---|
| Node `config` | New key → re-run |
| Upstream output (e.g. new git SHA) | Downstream keys change → re-run chain |
| Node type's `TypeVersion` (code update) | All entries of that type invalidate once |
| External input via `InputCacher` (ref moved) | That node + downstream re-run |
| Nothing, but `.afon/` was deleted | Local miss → re-run, **unless** the registry pre-check matches (image-build) |
| Nothing, but 24h passed | Nothing — Afon has no time-based expiry today (scan nodes will add a DB/TTL dimension when `trivy-scan` lands) |

## Practical tips

- **Force a rebuild:** delete `.afon/cache` (or the whole `.afon/`).
- **CI:** cache the `.afon/cache` directory between runs keyed on nothing
  special — it is safe to share; keys are self-validating.
- **Multi-machine:** the registry pre-check makes cold local caches
  cheap for builds; other node types have no remote cache yet.
- If a cached result ever looks wrong, it is always a *key computation*
  question — inspect the node's config and `TypeVersion` first.

Next: [Dependencies and outputs](dependencies-and-outputs.md).
