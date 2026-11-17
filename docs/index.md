# Afon

**A cluster-aware delivery pipeline for Kubernetes and k3s.**
Pluggable engine, local-first CLI, git to verified deployment.

Afon connects to an existing cluster, maps its active infrastructure, and
runs an end-to-end delivery pipeline from a flow file to a verified running
deployment — with a read-only `plan` in between, always.

> [!NOTE]
> **Early, learning project.** `inspect`, the flow engine, the Kubernetes
> driver, release records with rollback, `git-source` and `image-build`
> exist today. The plugin runtime and web UI are roadmap items. See the
> [Roadmap](ROADMAP.md).

## 60 seconds, no cluster

```bash
afon validate -f examples/fake-driver/flow.yaml   # parse + compile
afon plan    -f examples/fake-driver/flow.yaml    # fake driver: shows will-run / create
afon apply   -f examples/fake-driver/flow.yaml    # runs against local fake state
afon plan    -f examples/fake-driver/flow.yaml    # now cached / no changes
```

## Where to go next

| I want to… | Go to |
|---|---|
| Understand what Afon is and when to use it | [Overview](getting-started/overview.md) |
| Install it and run "Hello World" | [Installation](getting-started/installation.md) → [Quickstart](getting-started/quickstart.md) |
| Learn the mental model (actions, plans, cache, releases) | [Concepts](concepts/actions-and-resources.md) |
| Write my own flow | [Write a flow](guides/write-a-flow.md) |
| Copy a working example | [Recipes](guides/recipes.md) |
| Look up a command, flag, node or schema field | [CLI reference](reference/cli.md) |
| Fix an error I hit | [Troubleshooting](guides/troubleshooting.md) |

## Documentation map

```
getting-started/   onboarding: requirements, install, quickstart, project tour
concepts/          the mental model: how Afon thinks about delivery
guides/            how-to: author flows, recipes, local dev, troubleshooting
reference/         lookup: CLI, schema, node types, release records, glossary
design.md          design decisions and their history (kaniko pivot, PSA, …)
ROADMAP.md         what is built, what is planned, and the phase plan
```

## Viewing these docs locally

The site is plain markdown served by [docsify](https://docsify.js.org/) —
no build step:

```bash
npx docsify-cli serve docs     # then open http://localhost:3000
```

or read the `.md` files directly on GitHub.
