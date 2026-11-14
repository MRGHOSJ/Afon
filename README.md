# Afon

**A cluster-aware delivery pipeline for Kubernetes and k3s, driven by a pluggable engine and an interactive visual UI.**

Afon connects to an existing cluster, maps its active infrastructure, and runs an end-to-end delivery pipeline from git to a verified running deployment. Instead of rewriting or replacing your favorite infrastructure utilities, Afon provides a unified framework to drop in third-party tooling—such as security scanners, build engines, and GitOps agents—driven by a local CLI and visualized inside an interactive web control plane.

> **Status: early, learning project.** `inspect`, the flow engine (`validate` / `render` / `plan` / `apply`), the Kubernetes driver — server-side plan/apply, owner-scoped + inventory pruning, readiness waiting, delete confirmation — and release records with `history` / `status` / `rollback` exist today. Pipeline nodes and the visual web control plane are still on the roadmap. See [Status](#status) for details.

---

## Why Afon exists

Afon was born out of a desire to deeply understand the internals of Kubernetes delivery systems, Go concurrency mechanics, and low-level resource streaming via the Kubernetes API.

The project explores a highly distinct philosophy meant to combat DevOps toolchain fatigue:

1. **Adapt to the cluster you have:** Most self-hosted deploy platforms expect a completely blank canvas. Afon probes your active cluster parameters first (ingress configurations, active storage topologies, custom CRD states, and user permissions) and compiles execution plans tailored to that live infrastructure.
2. **Pluggable Node Ecosystem:** Instead of locking you into a proprietary ecosystem, Afon acts as an open framework. You can cleanly plug in external tools like **Trivy, ArgoCD, or custom testing binaries** through language-agnostic executable/gRPC interfaces. 
3. **Local-First with a Visual Control Plane:** Run everything locally from your development terminal or type `afon ui` to spin up a single-binary visual canvas. You can transition smoothly from a pure text CLI into a real-time interactive dashboard mapping out your multi-cluster infrastructure and pipeline graphs.
4. **Cluster-Aware Pre-flight Guarantees:** Pipelines evaluate real physical cluster boundaries—such as calculated **scheduling headroom** and custom RBAC access reviews—failing workflows safely during the `plan` stage before trying to apply invalid configurations.

---

## Status

| Area | State |
|---|---|
| `afon inspect` (cluster profiling engine) | **Implemented** |
| `flow.yaml` parser, validation layer & DAG engine | **Implemented** (no cluster: `validate`, `plan`, `apply` on a fake driver) |
| Materialized manifest rendering (`afon render`, `--check`) | **Implemented** |
| Kubernetes driver: SSA plan/apply, owner-scoped prune, readiness waiter, delete prompt, plan header | **Implemented** (k3d/kind/k3s) |
| Release records, health-gated rollback (`history`/`status`/`rollback`) | **Implemented** (k8s driver) |
| Pipeline nodes (`git-source`, `image-build`, `trivy-scan`, Helm) | Planned |
| Language-agnostic plugin & extension runtime | Planned |
| Integrated Web UI & real-time topology map (`afon ui`) | Planned |
| Multi-cluster aggregated control plane agent | Planned |

---

## What works today: `afon inspect`

`inspect` is read-only. It connects using your active kubeconfig, builds a structured **cluster profile** of facts, and streams either a text summary or raw JSON.

```bash
afon inspect                 # text summary for the current context
afon inspect -n my-app       # target a specific namespace
afon inspect -o json         # output full profile as JSON
afon inspect --full          # include raw /readyz endpoints and full API lists
afon inspect --redact        # scrub server URLs, UIDs, node names, and IPs
```

## What works today: flow engine (no cluster)

Phase 2 runs the flow pipeline end to end against a **fake driver** — no Docker, no cluster. State (action cache + fake objects) lives in `.afon/` next to the flow file, so reruns across processes are real no-ops.

```bash
afon validate -f flow.yaml   # parse + compile: types, references, cycles
afon plan -f flow.yaml       # read-only preview: cached/will-run, create/update/none
afon apply -f flow.yaml      # re-plan, then execute; banner confirms no cluster is touched
```

Plan statuses: actions report `cached` or `will-run`; resources report `create`, `update`, `delete`, `none`, or `known-after-apply` when an upstream action's outputs don't exist yet. A plan's `will-run` is an upper bound — apply reports what actually happened, with a per-node detail after the status: `succeeded (cached)`, `succeeded (created ConfigMap/hello)`, or `succeeded (no changes)`.

```bash
afon plan -f flow.yaml --parallelism 8   # cap concurrent nodes (default 4)
afon apply -f flow.yaml --state-dir /s   # override state location (default: flow file's dir)
afon plan -f flow.yaml -o json           # machine-readable plan
```

## What works today: Kubernetes (render → plan → apply)

The same engine drives a real cluster through the Kubernetes driver. Work through the runnable example: **[`examples/hello-app`](examples/hello-app)**.

```bash
afon render -f examples/hello-app/flow.yaml          # materialize generated.yaml (provenance header)
afon render -f examples/hello-app/flow.yaml --check  # exit 1 if stale; prints the would-be content
afon plan  -f examples/hello-app/flow.yaml --driver k8s --context k3d-dev
afon apply -f examples/hello-app/flow.yaml --driver k8s --context k3d-dev
```

What to know before the first apply:

- **Plan is read-only** — server-side dry-run classification plus a label-scoped LIST for pruning (owner labels `afon.dev/flow` / `afon.dev/node`; objects owned by another flow are refused, never adopted). The previous release's inventory covers the rest: objects the flow no longer owns — including a kind removed entirely from the flow — are deleted after a live read confirms them (disable both halves with `--no-prune`).
- **Text mode prints a header** on stdout after planning: `driver: k8s context: … namespace: … force: off prune: on`. A one-line banner on stderr (`k8s driver: context=… namespace=…`) prints earlier, *before* connecting — it is the only context signal in `-o json` mode and when discovery fails, so both exist on purpose.
- **Deletes ask first.** If the plan contains deletes, apply prompts `plan deletes N object(s); apply these changes? [y/N]` (N = actual objects, not plan entries). Non-interactive stdin (pipes, CI) refuses instead of guessing — pass `--auto-approve` to proceed.
- **Apply waits for readiness** — Deployment/StatefulSet/DaemonSet rollouts, Job completion, Pod containers. Default timeout 5m (`--wait-timeout`); `0` means no deadline (the context bounds it); `--no-wait` skips.
- **Namespace** comes from `spec.target.namespace` in the flow (required for the k8s driver); `--context` overrides `spec.target.context`.

## What works today: release records and rollback

Every apply writes an immutable release record (`afon.release.<flow>.v<N>`, a Secret in the target namespace, last 10 kept). Records need the k8s driver; other drivers refuse with `release records require the k8s driver (pass --driver k8s --context ...)`.

```bash
afon history -f flow.yaml --driver k8s --context k3d-dev   # newest-first: SEQ STATUS TRIGGER AGE OBJECTS
afon status  -f flow.yaml --driver k8s --context k3d-dev   # latest release vs live: missing / recreated / changed / extra
afon rollback -f flow.yaml --driver k8s --context k3d-dev  # prompt-confirm replays the previous release; --to K picks one
```

- **`status`** diffs the newest comparable record's inventory against live objects and stops with an info line when the latest run is still pending or failed.
- **`rollback --to K`** dry-runs first, replays K's recorded manifest streams as resource nodes (prompt-confirm for deletes, like apply), and appends a new `rolledback` record — nothing is overwritten. `--to 0` (default) picks the newest restorable release before the latest.
- **Automatic rollback:** a node with `onFailure: rollback` arms it — when any node fails (a readiness wait fails the node too), the previous release is restored automatically with a `rolling back to release K` notice on stderr, no prompt, and a `rolledback` record.
- **Crash detection:** a fresh `pending` record blocks a concurrent apply (wait or `--takeover`); a stale one (>10 min) is reported as a probable crashed run and superseded by `--takeover`.
- **Limits:** rollback restores recorded manifests only — not database migrations, volume contents, CRD schema changes, or anything outside the cluster (design.md §3).

### What the profile contains

| Section | Facts |
|---|---|
| `identity` | Context, server, cluster UID, flavor (distribution, version suffix, provider ID), environment (for example k3d) |
| `version` | Server version, client-go version, minor-version skew |
| `discovery` | API groups and resource counts, plus detected capabilities (Traefik, Gateway API, cert-manager, Argo CD, Flux, Kyverno, metrics-server, and others) |
| `health` | `/readyz` result, CoreDNS readiness, `kube-system` workload readiness |
| `nodes` | Per node: capacity, allocatable, conditions, taints, labels, runtime, pod CIDRs. Cluster and per-node requested resources and **scheduling headroom** |
| `access` | Full rules for the target namespace, plus 32 curated permission checks for what a deploy pipeline needs (apply, prune, wait, release records, logs) |
| `namespace` | Existence, phase, labels (including Pod Security), quotas, limit ranges |
| `networking` | Ingress classes (and which is default), Gateway API classes, gateways and routes, network policies, CNI hints |
| `storage` | Storage classes (and default), PVs, PVCs |
| `sections` | A status for every section: `ok`, `partial`, `denied`, or `unavailable` |

### Design principles of the profile

- **Facts only:** The profile records concrete parameters (e.g., "ingress classes: traefik (default)"). It never makes subjective judgments; evaluation and rule checking belong entirely in higher validation layers.
- **Structured failure:** Every cluster section has an isolated state, and errors are mapped as structured `ProbeError` signatures. "No ingress controller found" and "unauthorized to read ingress controller" are split into different explicit outcomes.
- **`[]` versus `null`:** An empty list indicates a resource was verified but none exist. A null value indicates that specific domain area was skipped or not probed.
- **Per-namespace by default:** Cluster-wide exploration is completely opt-in, preserving pipeline execution speed and respecting standard namespace boundary limitations.
- **Scheduling headroom is not usage:** Headroom represents the cluster's allocatable capacity minus existing pod requests. It maps exactly what size workload can realistically be scheduled next, rather than simple active RAM/CPU consumption.
- **Safe to share:** Labels and annotations filter through an explicit allowlist (preventing the leakage of underlying master server commands), timestamps utilize standard UTC formatting, and the `--redact` flag automatically strips network identifiers, IPs, and host names.

---

## The Delivery Architecture (Roadmap)

### Core Engine Concept

```
flow.yaml → validate → plan → apply → wait ready → release record → (onFailure: rollback)
              │          │       │
              │          │       └─ concurrent execution via Go DAG channels
              │          └─ server-side dry-run diffing & headroom confirmation
              └─ strict schema checks, input/output validation, and cycle detection
```

A **flow** is a collection of modular **nodes** containing execution dependencies. Nodes fall into two distinct structural types governed by their underlying contract:

- **Action Nodes:** Execute commands and produce outputs (`git-source`, `image-build`, `trivy-scan`). Action nodes are completely content-addressed: if their explicit inputs remain unchanged, Afon flags them as `CACHED` and skips execution.
- **Resource Nodes:** Represent actual desired cluster states (`k8s-apply`). They process structural differences against live infrastructure using the API server's native server-side dry-run flags.

Outputs from Action nodes dynamically feed Resource configurations (e.g., matching a container build digest to a target manifest schema), keeping deployments locked to explicit hashes.

### Flow File Specification (`flow.yaml`)

```yaml
apiVersion: afon.dev/flow/v1
kind: Flow
metadata:
  name: hello-world
spec:
  target:
    context: k3d-dev          # target kubeconfig context
    namespace: hello
  nodes:
    - id: security-scan
      type: extension/trivy   # pluggable node configuration
      config:
        severity: HIGH,CRITICAL

    - id: image
      type: image-ref         
      config:
        ref: docker.io/library/nginx:1.27

    - id: manifests
      type: k8s-manifests
      config:
        paths: [./deploy]
        images:
          app: ${image.ref}   # dynamic container output substitution

    - id: apply
      type: k8s-apply
      config:
        manifests: ${manifests.rendered}
        wait: { timeout: 120s }
      onFailure: rollback     # restore the previous release if this node fails
```

### Pluggable Extension Design
Extensions function as decoupled, language-agnostic binaries matching a strict input/output execution contract. By utilizing a unified payload model over gRPC or standard stdout pipes, you can develop your own automation scripts or wire up native tools (like Argo workflows) without altering the underlying Afon compiler.

### Interactive User Interface (`afon ui`)
Running `afon ui` boots an embedded web server that presents a full-stack, visual visualization of your deployment lifecycles.
* **DAG Visual Designer:** Drag, drop, and link custom plugin nodes together on a visual canvas to automatically compile the underlying `flow.yaml` specification.
* **Live Infrastructure Grid:** Monitor multi-dimensional cluster graphs displaying active namespaces, live pod transitions, and physical hardware node mapping in real time over persistent WebSockets.

---

## Comparison with Existing Paradigms

Afon occupies a unique intersection within the modern infrastructure management space:

* **vs. Local Task Engines (Dagger, Earthly):** While these systems are highly efficient at container isolation and building software locally, they lack native, deep cluster awareness, live target validation, and deployment lifecycle tracking.
* **vs. GitOps Controllers (ArgoCD, Flux):** These production engines operate strictly inside clusters and are complex to manage on a local machine. Afon delivers a lightweight, CLI-first approach that can either orchestrate your apps locally or smoothly generate ("eject") native custom resources directly out to your existing enterprise GitOps frameworks.
* **vs. Internal Developer Platforms (Backstage, Port):** Instead of imposing a rigid, heavy platform portal, Afon offers an open, single-binary CLI layout that can instantly wrap your existing developer tooling in clean visual feedback.

## License

Copyright 2026 MRGHOSJ

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

Full text: [LICENSE](LICENSE)

