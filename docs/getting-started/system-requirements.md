# System requirements

## Supported operating systems

| OS | State |
|---|---|
| Linux | Primary dev/CI target (`go vet` + `go test` run in CI) |
| Windows | Developed and used daily (PowerShell examples appear in docs) |
| macOS | Expected to work — pure Go, no OS-specific code paths |

Afon is a **single static binary**. There are no runtime dependencies once
it is built.

## Building from source

| Requirement | Version |
|---|---|
| [Go](https://go.dev/dl/) | **1.27.3** or newer (the module declares `go 1.27.3`) |
| git | Any recent version (also needed at run time for `git-source` flows) |

## Running against a real cluster

| Requirement | Why |
|---|---|
| A Kubernetes cluster (≥ 1.2x) | Needs **server-side apply** (stable since 1.22) and Pod Security Admission |
| [kubectl](https://kubernetes.io/docs/tasks/tools/) | Not required by Afon itself, but essential for debugging and for the manual hints Afon prints |
| A kubeconfig with a named context | `afon --context <name>` or `spec.target.context` in the flow |
| Docker (or Rancher Desktop) | Only if you use [k3d](https://k3d.io/) for a local cluster |

Verified against **k3d** locally; kind and k3s are expected to work (same
API surface). The [Quickstart](quickstart.md) uses k3d.

## Network access

| Flow feature | Needs |
|---|---|
| `git-source` | Reachability to your git host (`git ls-remote`) at plan and apply time. A full 40/64-hex SHA as `ref` skips the network entirely. |
| `image-build` | Push access to your registry (via a `kubernetes.io/dockerconfigjson` Secret) and pull access for the pinned builder images (`ghcr.io/osscontainertools/kaniko`, `alpine/git`) |
| `image-ref`, `fake-object`, `k8s-manifests`, `app-manifests` | No network at plan time |
| Viewing these docs | The docsify shell loads its assets from a CDN when served as a site (reading the markdown on GitHub needs nothing) |

## Cluster permissions (RBAC)

Afon needs, at minimum:

- **Target namespace:** full CRUD + `watch` on the objects in your flow
  (server-side apply, readiness watching, pruning), plus `get/list/create`
  on `namespaces` (apply auto-creates a missing target namespace).
- **Build namespace** (default `afon-build`, created automatically with
  Pod Security `baseline` labels): create/get/list/delete for Jobs and Pods,
  so `image-build` can run its throwaway builder.
- **Release records:** get/list/create/update/patch on Secrets in the
  target namespace — Afon manages *its own* release Secrets. It **never
  reads the contents of credentials Secrets** you create (they are
  referenced by name and mounted into Jobs).

If a permission is missing, Afon fails with a directive (for example
`create it manually: kubectl create namespace …`) rather than retrying
silently.

## Hardware

No special requirements: Afon itself is a few dozen MB of memory. Build
Jobs run in your cluster (kaniko), not on your machine.

## Terminal and shell

Works in any terminal. Interactive features used today:

- **Delete confirmation prompt** on `apply`/`rollback` when the plan
  contains deletes (skip with `--auto-approve`).
- **Ctrl-C** cancels cleanly: in-flight nodes get `cancelled`, the release
  record becomes terminal `failed`, and the process exits **1**.

Next: [Installation](installation.md).
