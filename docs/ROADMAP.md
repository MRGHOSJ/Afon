# Afon roadmap

Part-time pacing, about 12 weeks to a demoable project. Each phase ends with something you can run. Adjust the weeks to your schedule, but keep the order.

**Status key:** ✅ done · 🔨 in progress · ⬜ not started

## Phase 0: Setup and design (week 1)

- ✅ Repo, package layout, k3d dev cluster
- ✅ `docs/design.md`: action vs. resource, ClusterProfile, release record, flow schema
- ✅ CI running `go vet` and `go test ./...`

**Done when:** the design doc exists and you can explain what a plan means for each node kind.

## Phase 1: `inspect` (weeks 2-3)

- ✅ Cluster profile with structured section status
- ✅ Text summary, `-o json`, `--full`, `--redact`, `-n`
- ⬜ Table tests for pure functions: `subHeadroom`, `filterCapacity`, `allowMapKey`, flavor detection, IPv6 and MAC masking, `classify`
- ⬜ Hardening runs: restricted service account, `-n does-not-exist`, pod list denied
- ⬜ Second cluster type (kind), diffed against k3d
- ⬜ Redacted profiles saved to `testdata/`

**Done when:** `inspect` is correct and tested on two cluster types, and the `denied` and `partial` paths have been seen live.

## Phase 2: Flow core, no cluster (weeks 4-5)

- ✅ `pkg/schema`: parse and validate flows, resolve `${...}` references, errors with line numbers
- ✅ `pkg/graph`: DAG, topological sort, cycle detection
- ✅ Node registry, `image-ref` action, `fake-object` resource node
- ✅ Executor with `errgroup`: parallel scheduling, halt on failure, context cancellation
- ✅ Action cache: canonical hashing and invalidation (disk-backed, atomic writes)
- ✅ Plan model and renderer (`will-run`/`cached`, `create`/`update`/`none`/`known-after-apply`)
- ✅ In-memory fake driver (`.afon/fake-state.json` for the CLI) so the whole loop runs without Docker or a cluster
- ✅ Apply detail output (`created|updated Kind/Name`, `no changes`), aligned status columns, and coverage-gap tests: cancellation, `-o json`, update journeys, reset

**Done when:** `validate`, `plan`, and `apply` work end to end against the fake driver, a failed node leaves consistent state, and the graph, executor, and cache logic have strong tests. **Met** — all green under `go test -race ./...`, including the cross-process apply-twice-then-plan e2e.

## Phase 3: Kubernetes driver (weeks 6-7)

- ✅ `k8s-manifests` renderer (plain YAML; Kustomize after)
- ✅ Plan via server-side dry-run, apply via server-side apply with an Afon field manager
- ✅ Pruning by owner labels (same-kind removal), plus inventory-driven prune (kind-removal, Phase 4)
- ✅ Readiness waiting (engine gate policy + `driver.Waiter`)
- ✅ Context and namespace shown in every plan
- ⬜ Confirmation for non-local contexts (the delete-confirmation prompt exists; a context warning does not)
- ⬜ Profile feeds plan: first two checks ("route requested but no ingress class", "apply needs `patch` but access denied")

**Done when:** you can deploy a sample app to k3d, rerun for a no-op plan, change it for a correct diff, and remove a manifest and watch it pruned safely. **Met** for same-kind removal — and the kind-removal orphan gap is closed by the release-record inventory (design.md §5.4).

## Phase 4: Releases and rollback (week 8)

- ✅ Release records as in-cluster Secrets, with a `Backend` interface
- ✅ `history`, `status`, `rollback --to`
- ✅ `pending` marker for crash detection and concurrency
- ✅ Auto-rollback (`onFailure: rollback`) after a failed node, including readiness-gate failures
- ✅ Rollback limits documented (design.md §3)

**Done when:** you can deploy v1, deploy a deliberately broken v2, watch it roll back automatically, and see both in `history`. **Met** — live on k3d: v1 succeeded, v2 failed its readiness wait, the run printed `rolling back to release 1`, restored the ConfigMap, deleted the broken Deployment (failed records inventory what they applied), and `history` shows v1 succeeded / v2 failed / v3 rolledback.

## Phase 5: Pipeline nodes (weeks 9-10)

- ⬜ `git-source` (resolved commit SHA is the cache input)
- ⬜ `image-build` (BuildKit or Docker, push to a local registry, resolve the digest)
- ⬜ `trivy-scan` (severity threshold, allowlist, DB-version or TTL cache dimension)
- ⬜ Helm renderer
- ⬜ Secrets handling and log masking
- ⬜ `notify-webhook` and a "how to write a node" doc

**Done when:** one flow goes from a git repo to a scanned, deployed, health-checked release on k3d, and a failing scan blocks the deploy.

## Phase 6: Simple spec and polish (week 11)

- ⬜ Short spec that expands into the full flow, plus `eject`
- ⬜ Plain-language plan output
- ⬜ Translated errors for CrashLoopBackOff, ImagePullBackOff, and a missing ingress

**Done when:** a stranger can write the short spec for a sample app and deploy it by following the README.

## Phase 7: Demo and docs (week 12)

- ⬜ README split into **Implemented** and **Roadmap**, design-decisions section, honest comparison
- ⬜ 2-3 minute terminal recording: inspect, plan, apply, broken deploy, automatic rollback
- ⬜ End-to-end test on kind in CI

## Stretch goals (pick by interest)

1. Argo CD and Flux awareness: sync through them where they manage a namespace
2. `serve` mode with webhook triggers and run history
3. Installer nodes for missing components (cert-manager, ingress)
4. k3s bootstrap over SSH
5. Preview environments per branch
6. Web view of the flow with live node status
7. In-cluster controller with a `Flow` CRD

## Checkpoints

| End of | You should have |
|---|---|
| Week 3 | A useful, tested standalone `inspect` command |
| Week 5 | A tested engine with plan and apply on a fake driver |
| Week 8 | Real deploys to Kubernetes with rollback |
| Week 10 | A full git-to-running pipeline |
| Week 12 | A polished, documented, demoable project |

## Rules to keep it on track

- **Tests as you go.** The graph, cache, probe rules, and release logic are where the learning sticks, so cover them best.
- **Cut from the bottom.** If you fall behind, drop phase 6 polish or Helm before touching `inspect`, the engine, or rollback.
- **Don't advertise unbuilt features.** If it isn't merged, it stays under roadmap.
- **Keep a design log.** Write down each decision and mistake. It becomes interview material.
- **Dependencies point one way.** `probe` never imports `engine`, and `engine` never imports Kubernetes types.
- **No more probe sections before `pkg/checks` has a consumer.** Let real checks decide what to probe next.
