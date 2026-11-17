# Installation

Afon ships as a single binary. Prebuilt downloads are **not published
yet** — build from source.

## Build from source

```bash
git clone https://github.com/MRGHOSJ/afon.git
cd afon
go build -o afon ./cmd/afon
```

On Windows (PowerShell):

```powershell
git clone https://github.com/MRGHOSJ/afon.git
cd afon
go build -o afon.exe ./cmd/afon
```

## Put it on your PATH

Linux / macOS:

```bash
sudo mv afon /usr/local/bin/afon     # or: ~/.local/bin
```

Windows (PowerShell, current user):

```powershell
Move-Item afon.exe "$env:USERPROFILE\bin\afon.exe"   # if that dir is on PATH
```

Or simply keep the binary in your project folder and call `./afon` /
`.\afon.exe`.

## Verify

```bash
afon --help
afon inspect --help
```

`afon --help` lists every subcommand. If you see usage text, you are done.

## Optional: k3d for a local cluster

Everything in the [Quickstart](quickstart.md) except the fake-driver path
needs a cluster. The lightest local option:

```bash
# install k3d (https://k3d.io), then:
k3d cluster create dev
kubectl cluster-info    # sanity check with kubectl
```

Your kubeconfig gains a context (e.g. `k3d-dev`) — pass it to Afon as
`--context k3d-dev`.

## Uninstall

Delete the binary. Optionally remove local state (cache, fake-driver
state) — it lives next to your flow files, see
[Project structure](project-structure.md):

```bash
rm -rf <flow-dir>/.afon
```

Nothing is installed globally besides the binary, and no cluster objects
exist until you run `apply` with `--driver k8s`.

## Next

- [Quickstart](quickstart.md) — 5-minute end-to-end walkthrough
- [Recipes](../guides/recipes.md) — copy-paste flows for every node type
