# easydev-platform

The **complete platform** that combines the three core servers into one
deployable product, while each server **stays independently deployable**:

| Component | Role | Reused as |
|---|---|---|
| **easyvcs** | revision-native VCS (drafts, RRs, merges, web UI) | **Go library** (`server.New`) |
| **easyblobs** | 21-protocol package registry + OCI | **Go library** (`core`) or sidecar |
| **easyops** | Kubernetes / BuildKit ops (sandboxes, jobs, builds) | **network** (Connect `OpsService`) |

## What the platform adds

1. **One identity** — a single `whoami`/token authority shared by every
   component (easyvcs owns users+tokens; easyops verifies user tokens against
   it; easyblobs resolves identities the same way).
2. **One gateway** — a single origin that mounts `/vcs/*`, `/blobs/*` and
   `/ops/*`, so a browser or CLI sees ONE server.
3. **CI** — the piece none of the three has: `.easydev-ci.yml` pipelines,
   triggered by easyvcs events (RR opened/synced/merged), executed on easyops
   (Jobs / BuildKit), with results written back to easyvcs as **checks** that
   can block a merge.
4. **Helm chart** for k3s (Postgres + S3/Garage + buildkitd + the three
   servers + the platform).

## Layout

```
cmd/easydev-platform/   the single binary (gateway + CI orchestrator)
internal/gateway/       mounts the three components under one origin
internal/identity/      unified auth (one token authority)
internal/ci/            pipeline model, yml parsing, orchestration, checks
internal/store/         platform metadata (pipelines, runs, checks, secrets)
deploy/platform/        Helm chart (k3s)
docs/                   design notes
```

## Status

Scaffold: the gateway, unified identity and the CI orchestrator skeleton.
See `docs/architecture.md`.

## Building

The platform consumes the three core servers **as libraries/services**; it does
not vendor them.

```sh
# Development: with the repos checked out as siblings, go.work wires them so a
# local edit in ../easyvcs is picked up immediately.
go build ./...

# CI / Docker: the sibling checkouts are absent, so go.work is ignored and the
# modules are resolved from the module proxy (see go.mod's require + the
# replace directives, which point at the published module paths).
GOFLAGS=-mod=mod GOWORK=off go build ./...
```

> **Note.** The published module `github.com/easylab-platform/easyvcs` on the
> proxy is a much older snapshot (pre-`/api/v1`). Until a release that contains
> the current API is tagged, the Docker/CI build uses the **sibling checkout**
> (see `Dockerfile`: it copies `../easyvcs` into the build context). Once
> easyvcs tags a release, drop the sibling copy and the `replace` directives.
