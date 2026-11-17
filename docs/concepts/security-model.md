# Security model

This page collects the guarantees Afon makes about credentials, namespaces,
ownership, and output — the things you need to trust before pointing it at
a real cluster.

## Secrets: referenced by name, never read

**Afon never reads the contents of Secrets you create.**

- Credentials (registry push auth, git auth) are referenced **by name** in
  flow config (`registrySecret: dockerhub-creds`, `gitSecret: …`).
- The named Secret must live in the **build namespace** (default
  `afon-build`); the build Job *mounts* it — the kubelet delivers it to
  the container, not to Afon.
- Afon surfaces **pod events** (e.g. `FailedMount: secret "x" not found`)
  so you learn a secret is missing — but it never fetches secret data.
- There is deliberately **no** `afon secret get`/`list` command today.

The exception that proves the rule: Afon manages **its own release-record
Secrets** (`afon.release.*`) — it writes and reads those, because they are
Afon's data, not yours.

## Namespaces

| Namespace | Created by | Labels |
|---|---|---|
| **Target** (`spec.target.namespace`) | `apply` and `rollback` only — plain, no labels added | none added (existing labels untouched) |
| **Build** (default `afon-build`) | the `image-build` runner, on demand | `pod-security.kubernetes.io/{audit,enforce,warn}=baseline` |

Hard rules:

- **Existing namespaces are never modified.** Afon does not add labels or
  annotations to a namespace it didn't create.
- **`plan`, `validate`, `status`, `history` create nothing** — plan fails
  fast on a missing namespace with `create it before applying`.
- **Forbidden is terminal:** if the namespace create is denied by RBAC,
  the error includes the manual remedy:
  ```
  build namespace "afon-build" does not exist and afon cannot create it: ...
  (create it manually: kubectl create namespace afon-build)
  ```

## Pod Security: baseline, not privileged

Build Jobs run under Pod Security **baseline**:

- The pod sets `seccompProfile: RuntimeDefault` (accepted by baseline).
- No privileged escalation, no host namespaces, no capability additions.
- Kaniko's former `Unconfined` seccomp requirement was a BuildKit-era
  artifact; the pinned kaniko fork does not need it (see
  [design.md](../design.md) §6).

If admission ever refuses the Job, the error carries the hint to check
namespace labels — Afon does not attempt to relabel namespaces itself.

## Ownership: Afon only touches what it owns

Every object Afon applies carries owner labels (flow + node):

- **Plan refuses foreign objects** — if an object is already owned by a
  *different* flow/node, you get:
  ```
  Deployment/foo is owned by flow "other" node "deploy"; refusing to manage it from another owner
  ```
  No silent adoption, ever.
- **Prune is owner-scoped** — only objects with *this* flow's labels are
  candidates for deletion when removed from the flow.
- **Field conflicts** fail the apply unless you opt in:
  ```
  fields owned by ...; re-run with --force-conflicts to take ownership
  ```

## Cluster-scoped objects are refused

The k8s driver manages exactly one namespace. Cluster-scoped resources
(Namespace, ClusterRole, CRD, …) are rejected at plan time — Afon will
not half-manage cluster scope.

## Output redaction

- `afon inspect --redact` masks identifiers and addresses in profiles.
- Node args like `k3s.io/node-args` and node env are never emitted from
  inspect; labels/annotations are allowlisted before being printed.
- `afon plan -o json` / `history -o json` print flow state, not secrets.

## RBAC footprint (summary)

| Area | Operations Afon needs |
|---|---|
| Target namespace objects | server-side apply (create/patch), get/list/watch (diff, wait, prune) |
| Namespaces | get, create (auto-create path only) |
| Build namespace | create/get/list/delete Jobs; get/list/watch Pods; get Events (error surfacing) |
| Release Secrets (target ns) | get/list/create/update/patch/delete — Afon's own records |

Principle of least privilege still applies on your side: give the flow's
identity the namespace-scoped role it needs, nothing cluster-wide.

## What Afon does *not* do (yet)

- No encrypted secret store of its own; no SOPS/age integration.
- No signature verification of images or git commits.
- No secret redaction in `image-build` build args (don't pass secrets as
  build args — mount them).
- Extension runtime (third-party nodes) is roadmap — when it lands, its
  trust model will be documented here.

Next: [Write a flow](../guides/write-a-flow.md).
