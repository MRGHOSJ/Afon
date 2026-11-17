# Glossary

**Action**
A node kind that runs something and produces outputs. The only part of
the engine with side effects — which is what makes plans read-only.
See [Actions and resources](../concepts/actions-and-resources.md).

**Apply** (`afon apply`)
Re-plan, then execute: run actions, apply resources, wait for readiness,
write a release record.

**Build namespace** (`afon-build`)
Namespace where `image-build` runs its throwaway Job and where
credential Secrets live **by name**. Auto-created with Pod Security
`baseline` labels.

**Cache key**
`sha256(canonical config + external inputs + TypeVersion)` — the
content-addressed identity of an action's result. Same key → `cached`.

**ClusterProfile**
The structured, read-only map of a cluster produced by `afon inspect`.

**Content-addressing**
Storing/caching results by a hash of their inputs rather than by time or
name.

**Content key**
The hex value stamped into pushed images as the
`afon.dev/build-config` label; equal to the image-build content hash, so
an identical rebuild is detectable by anonymous registry probe.

**Digest-pinned reference**
`repo@sha256:…` — an immutable image reference. `image-build` publishes
one as its `ref` output; deploys consume it.

**Driver**
Where execution happens: `fake` (local state file) or `k8s` (real
cluster). Never read from the flow file — always the `--driver` flag.

**Flow**
A YAML file (`flow.yaml`) declaring nodes and an optional target.
Format: [Flow schema](flow-schema.md).

**Headroom**
Schedulable capacity shown by `inspect` — the pre-flight guarantee that
your plan fits the cluster.

**Inventory**
The list of live objects (identity + owner + content hash) a release
record claims; what `status` compares and rollback prunes by.

**Materialized output**
A node output written to a file (e.g. `generated.yaml`) with a
provenance header; `plan`/`apply` read it as-is, `render` writes it.

**Node**
One step in a flow: an action or a resource. Types are registered in
code; users write only `type:` + `config:`.

**Owner labels**
Labels Afon stamps on applied objects (flow + node) — the basis of
scoped pruning, rollback, and refusing foreign objects.

**Pending**
Release-record state meaning "an apply is (or was) in flight" — the
concurrency guard; also the crash marker before `--takeover`.

**Plan** (`afon plan`)
The read-only diff: what apply would do. Statuses: `will-run`, `cached`,
`create`, `update`, `none`, `known-after-apply`.

**Pod Security Admission (PSA) baseline**
The security profile Afon's build Jobs and build namespace use — no
privilege, RuntimeDefault seccomp.

**Prune**
Deleting live objects that were removed from the flow — owner-scoped,
on by default, disabled with `--no-prune`.

**Recipe**
A copy-paste example flow. See [Recipes](../guides/recipes.md).

**Reference** (`${...}`)
The `${node.output}` / `${secret.NAME}` / `${env.NAME}` grammar linking
nodes. See [Reference grammar](reference-grammar.md).

**Release record**
One numbered account of an apply, stored as Secret
`afon.release.<flow>.v<seq>`. States: pending/succeeded/failed/rolledback.

**Resource**
A node kind that declares objects which should exist; diffed against
live state, never "runs".

**Rollback** (`afon rollback`)
Re-applies a previous release's **recorded manifests** and records
itself as a new release (append-only history).

**Server-side apply (SSA)**
The Kubernetes API mechanism Afon uses to apply objects with a field
manager; conflicts surface as `--force-conflicts` decisions.

**Server-side dry-run**
The read-only API call plan uses to compute diffs without persisting.

**Takeover** (`--takeover`)
Crossing the 10-minute stale-pending window to recover from a crashed
run. Fresh pending records always block.

**TypeVersion**
A node type's behavior version, part of every cache key — bump it and
that type's caches invalidate once.

**will-run / cached**
Plan statuses for actions: cache miss (will execute) vs cache hit
(skipped on apply).
