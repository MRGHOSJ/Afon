# Overview

Afon is a **cluster-aware delivery pipeline** for Kubernetes and k3s. You
describe *what should exist* in a flow file, and Afon plans the diff against
your live cluster, applies it, waits for health, records what went out, and
can roll it back.

## The problem it addresses

Delivery tooling tends to sprawl: one tool to build, one to scan, one to
deploy, one to watch, plus a pile of shell glue. Afon takes a different
approach — a single local CLI with a pluggable node engine, so third-party
tools (build engines, scanners, GitOps agents) drop in as nodes instead of
becoming separate pipelines.

## Core principles

| Principle | What it means |
|---|---|
| **Adapt to the cluster you have** | Afon probes the live cluster first (ingress, storage, permissions, headroom) and plans against what is actually there. |
| **Plan before apply** | `plan` is read-only *by construction* — only `Run` has side effects, so a plan can never mutate anything. |
| **Pluggable node ecosystem** | Nodes are registered types, not shell-out scripts. Pipeline nodes (`git-source`, `image-build`) work today; a scanner/extension runtime is on the roadmap. |
| **Content-addressed caching** | Unchanged work is skipped. If inputs are identical, the node is `cached` — unless external state (a moved git ref, a new CVE database) invalidates it. |
| **Honest state** | Release records live in the cluster as Secrets. `history` and `status` compare *recorded* state to *live* state; nothing is inferred. |

## Key vocabulary (30 seconds)

| Term | Meaning |
|---|---|
| **Flow** | A YAML file (`flow.yaml`) declaring nodes and an optional target cluster/namespace. |
| **Node** | One step. Either an **action** (runs something, produces outputs) or a **resource** (declares objects that should exist). |
| **Plan** | A read-only diff: what `apply` would do, with statuses like `will-run`, `cached`, `create`, `none`. |
| **Apply** | Re-plans, then executes: runs actions, applies resources, waits for readiness, writes a **release record**. |
| **Release** | One numbered, recorded apply (`v1`, `v2`, …) stored as a Secret in the target namespace. |
| **Driver** | Where execution happens: `fake` (local state file, no cluster) or `k8s` (a real cluster via kubeconfig). |

## When Afon is a good fit

- You want **git → image → deploy → verified** as one local, inspectable run.
- You have an existing cluster you cannot treat as a blank canvas.
- You value a **read-only plan** and recorded, rollback-able releases.
- You want pipeline nodes (build, scan, notify) in one engine rather than
  five YAML repos.

## When it is not

- You need in-cluster continuous reconciliation (GitOps controllers like
  Argo CD / Flux own that; Afon is CLI-first and local).
- You want a full developer portal (Backstage-style).
- You need Helm chart rendering — not implemented yet (see the
  [Roadmap](../ROADMAP.md)).

## Honest comparison

| Compared to | The difference |
|---|---|
| **Local task engines** (Dagger, Earthly) | Great at container isolation and local builds, but without native cluster awareness, live target validation, or deployment lifecycle tracking. |
| **GitOps controllers** (Argo CD, Flux) | These run inside clusters and are heavy to run locally. Afon is lightweight and CLI-first. |
| **Developer platforms** (Backstage, Port) | Instead of a heavy portal, Afon is a single binary that wraps your existing tooling with clean feedback. |

## The commands at a glance

| Command | What it does |
|---|---|
| `afon inspect` | Probe the cluster and print its profile (read-only). |
| `afon validate` | Parse and compile a flow file; touch nothing else. |
| `afon render` | Run action nodes and write materialized outputs to files. |
| `afon plan` | Show what an apply would do — without executing. |
| `afon apply` | Execute the flow against the driver. |
| `afon history` | List release records, newest first. |
| `afon status` | Compare live cluster state to the latest record. |
| `afon rollback` | Re-apply an earlier release's recorded manifests. |

Next: [System requirements](system-requirements.md) →
[Installation](installation.md) → [Quickstart](quickstart.md).
