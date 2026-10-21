# Architecture

```
                        ┌─────────────────────────────────────────┐
                        │           easydev-platform              │
                        │  (ONE origin: gateway + CI orchestrator)│
                        └───┬──────────────┬──────────────┬───────┘
                            │              │              │
             embedded(LIB)  │        proxy │       proxy  │  native
                            ▼              ▼              ▼
                      ┌──────────┐  ┌───────────┐  ┌───────────┐
                      │ easyvcs  │  │ easyblobs │  │  easyops  │
                      │ (VCS+UI) │  │ (21 protos│  │ (k8s/BK)  │
                      │          │  │  + OCI)   │  │           │
                      └──────────┘  └───────────┘  └───────────┘
```

**Each core server stays independently deployable.** The platform composes
them; nothing in easyvcs/easyblobs/easyops depends on the platform.

## Why a separate platform repo

| Concern | Where it lives |
|---|---|
| revisions, drafts, RRs, merge, web UI | **easyvcs** (unchanged) |
| package registry (21 protocols), OCI | **easyblobs** (unchanged) |
| sandboxes, services, PVCs, Jobs, image builds | **easyops** (unchanged) |
| **unified identity** (one token authority) | **platform** (wires them) |
| **one origin** (`/ui`, `/api/v1`, `/blobs`, `/ops`, `/ci`) | **platform** |
| **CI: triggers, pipelines, DAG, checks that block a merge** | **platform** |

## Identity

**easyvcs is the authority.** It owns users and tokens.

- The platform embeds easyvcs, so `/api/v1/users`, `/api/v1/tokens`,
  `/api/v1/whoami` are served by it.
- **easyops** is configured with `EASYOPS_EASYVCS_URL`; it verifies every user
  token against easyvcs `whoami` (and rejects a mismatched `owner`).
- **easyblobs** resolves identities the same way (`core/remoteauth.go`).

## CI

The missing piece. A pipeline is a `.easydev-ci.yml` at the head of a
Revision Request:

```yaml
version: 1
on: [rr_opened, rr_synced]
jobs:
  test:
    image: golang:1.27
    commands: ["go vet ./...", "go test ./..."]
    required: true          # blocks the merge unless green
  build:
    needs: [test]           # the DAG edge
    image: golang:1.27
    commands: ["go build ./..."]
    artifacts: ["bin/*"]
```

Flow:

```
RR opened/synced ──► POST /ci/events {namespace, trigger, ref, head, rr, yaml}
                       │
                       ▼
              ci.Orchestrator
                ├─ ParseYAML + Validate + Order (topological STAGES)
                ├─ for each stage, for each job (bounded parallelism):
                │     ops.RunJob  → easyops CreateJob (batch/v1.Job)
                │     ops.WaitJob → wait for the Job to finish
                │     ops.Logs    → capture the tail
                └─ store the Run + update the RR's CHECK
                       │
                       ▼
      GET /ci/checks?namespace=&rr=  →  {required_pending: true|false}
                       │
                       ▼
              the merge gate: required_pending ⇒ REFUSE the merge
```

### What is real and what is stubbed

| Piece | Status |
|---|---|
| pipeline model, YAML parsing, validation, DAG ordering | **done** (`internal/ci/pipeline.go`, `parse.go`) |
| execution loop (stages, parallelism, fail-fast, skip-downstream) | **done** (`orchestrator.go`) |
| checks + merge gate (`required_pending`) | **done** (`internal/gateway/ci_http.go`) |
| durable store | **in-memory** (`ci.MemStore`) — a Postgres impl is a drop-in |
| ops backend | **fake** by default — the real one is `easyops.v1.OpsService` over Connect (`CreateJob`/`JobLogs`/`DeleteJob`) |
| easyvcs → platform events | **manual** (`POST /ci/events`) — a native easyvcs webhook is the next step |
| artifacts upload to easyblobs | **declared** (`artifacts:` parsed) — upload is TODO |

## Roadmap (to GitHub parity)

1. **easyvcs webhook** — emit `rr.opened` / `rr.synced` / `rr.merged` / `tag.created`
   with an HMAC signature; the platform subscribes.
2. **Real easyops client** — generate the Connect client from
   `proto/easyops/v1/ops.proto` and replace `FakeOps`.
3. **Merge gate inside easyvcs** — `POST /rr/{iid}/merge` consults
   `/ci/checks?required_pending=` and returns 409 while red.
4. **Artifacts** — upload the job's `artifacts:` globs to easyblobs, link them
   from the run and the RR.
5. **Caching** — mount a BuildKit cache + a module cache PVC into CI jobs.
6. **Postgres store** — move `ci.MemStore` behind the same interface.
7. **Secrets** — per-namespace encrypted secrets injected as job env.
