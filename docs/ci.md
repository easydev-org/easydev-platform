# CI — workflows, actions and execution

EasyDev CI turns a file in a namespace into a DAG of container jobs. It is the
one thing easyvcs, easyblobs and easyops do not provide on their own:

| Concern | Owner |
|---|---|
| revisions, Revision Requests, merges, checks storage | **easyvcs** (unchanged) |
| packages, images, artifact storage | **easyblobs** (unchanged) |
| sandboxes, services, Jobs, PVCs, image builds | **easyops** (unchanged) |
| workflow parsing, DAG, matrix, scheduling, checks | **this platform** |

The VCS stays a pure engine: it stores revisions, and the platform reads them.
The merge gate lives in the gateway, not in easyvcs.

---

## 1. The coordinate of a run is a snapshot, not a branch

```
Run = (namespace, workflow_path, workflow_sha, ref, head, base)
```

`workflow_sha` is the **pinned snapshot** the workflow file was read from, and
`head` is the pinned snapshot the code is checked out at. Consequences:

- A run is **reproducible**: re-running it re-reads the same bytes.
- There is no "which version of the workflow ran?" ambiguity — the same problem
  that forces GitHub to have both `pull_request` and `pull_request_target`.
- A change **cannot rewrite CI to bypass CI**: for an RR event the workflow is
  read from the **base** snapshot (the target branch), so head code is measured
  by base rules. Merging is what changes the rules.

```
rr opened/synced ──► read .easydev/workflows/ci.yml @ BASE snapshot
                 ──► parse ──► expand matrix ──► DAG stages
                 ──► per JOB INSTANCE: CreateJob (all steps) ──► checks
```

## 2. Workflow file

`.easydev/workflows/<name>.yml`. The schema is deliberately close to GitHub
Actions so a team can migrate by renaming a few keys.

```yaml
version: 1
name: ci

on:
  rr:
    types: [opened, synced, merged]
    paths: ["src/**", "go.mod"]        # glob filter on changed paths
    branches: [main]                    # glob filter on the target branch
  tag:
    pattern: "v*"
  manual: true                          # workflow_dispatch
  schedule: ["0 3 * * *"]

permissions:
  contents: read
  packages: write
  checks: write

concurrency:
  group: ci-${{ easyvcs.namespace }}-${{ easyvcs.rr }}
  cancel-in-progress: true

env:
  GOFLAGS: -mod=readonly

jobs:
  test:
    runs-on: [linux, amd64]
    timeout-minutes: 30
    strategy:
      matrix:
        go: ["1.26", "1.27"]
      fail-fast: false
      max-parallel: 2
    container:
      image: easyblobs.local/library/golang:1.27
    services:
      pg:
        image: postgres:17
        env: {POSTGRES_PASSWORD: test}
        ports: [5432]
    steps:
      - uses: easydev-org/ci-actions/checkout@v3
        with: {ref: "${{ easyvcs.head }}"}
      - uses: easydev-org/ci-actions/setup-go@v3
        with: {go-version: "${{ matrix.go }}"}
      - name: vet
        run: go vet ./...
      - name: test
        if: ${{ always() }}
        run: go test -race ./...
    outputs:
      version: ${{ steps.meta.outputs.version }}

  publish:
    needs: [test]
    if: ${{ easyvcs.trigger == 'tag' }}
    runs-on: [linux]
    steps:
      - uses: ./actions/publish            # a LOCAL action in this checkout
```

The **v1 form is still accepted** — a job with `image:` + `commands:` becomes a
job with one `run` step, so existing `.easydev-ci.yml` files keep working.

### 2.1 Expression language

`${{ }}` is a small, side-effect-free subset — a hand-written lexer, recursive
descent parser and evaluator (`internal/ci/expr`, ~700 lines, no `eval`):

| Context | Members |
|---|---|
| `easyvcs` | `namespace rr ref tag head base trigger actor owner paths` |
| `github` | `repository ref sha event_name actor workflow` (compat alias) |
| `job` | `id key` |
| `matrix` | the combination's values |
| `needs` | `needs.<job>.result` / `needs.<job>.outputs.<k>` |
| `steps` | `steps.<id>.outputs.<k>` / `steps.<id>.outcome` |
| `env`, `secrets`, `runner` | as expected |

Functions: `contains`, `startsWith`, `endsWith`, `format`, `join`, `toJSON`,
`fromJSON`, `hashFiles`, `success()`, `failure()`, `cancelled()`, `always()`.

There is **no shell and no eval**, and an unparseable expression **fails the
run** rather than being emitted as raw text.

### 2.2 `use`-time safety

A value from `${{ }}` never lands raw in a shell command. It goes into an
environment variable (or is an action input); the script reads the variable.
That is what closes the classic "expression injection into `run:`" hole.

## 3. Actions: `uses: namespace/path@ref`

The syntax is GitHub's; the semantics fall out of the VCS model:

```
uses: easydev-org/ci-actions/setup-go@v3
      └─────┬─────┘ └─────┬────┘ └┬┘
       namespace        path      ref
```

- `namespace` is the VCS's only container; `path` is a **scope** inside it.
- Loading an action = fetching a **subtree archive at a pinned ref**:
  `GET /api/v1/namespaces/{ns}/archive/{ref}.tar.gz?path={path}`. No clone cache,
  no extra service, no marketplace.
- `@v3` may be a branch (mutable) or a **tag / revision id / snapshot sha**
  (immutable — easyvcs refuses to re-point a tag). Production workflows should
  pin a sha.
- `uses: ./path` is a local action inside the same checkout.

Supported runtimes, compiled **at load time** into a shell script so one step is
one container:

```yaml
# action.yml — shell action
runs: {using: shell, script: "..."}
# action.yml — composite action
runs:
  using: composite
  steps: [{name: a, run: "..."}, {name: b, run: "..."}]
```

`runs.using: node` / `docker` is **rejected with a clear error**. Composite
covers the same ground with a far smaller attack surface.

Inputs are validated: an undeclared `with:` key is an error (a typo cannot
silently do nothing), and a required input with no value fails the step.

## 4. Execution

```
orchestrator ──► stage (parallel instances) ──► ONE job instance = ONE backend job
                                                 │   (all of its steps, in order)
                                                 ├─ k8s batch/v1.Job (easyops)
                                                 ├─ Connect OpsService (production)
                                                 ├─ self-hosted runner (optional)
                                                 └─ FakeOps (tests / dry run)
```

`OpsClient` is the seam; adding a backend does not touch the orchestrator.

### 4.1 One job instance = one container (the supervisor)

A job's steps run **sequentially inside one container**, generated as a single
shell script. This is what makes the model behave like a runner:

- **`/workspace` is shared**, so "compile in step 1, test the artifact in step 2"
  works. (One executor job per *step* gave each step a fresh emptyDir, which
  made the most common pipeline impossible.)
- The checkout happens **once**, not once per step.
- Companion services are started once, not per step.
- There is no way for two steps to collide on an executor name.

The trade-off: Go can no longer intervene between steps, so anything that depends
on runtime state is decided **in the container**:

| Concern | How it is handled |
|---|---|
| `if:` | compiled to a POSIX shell gate (`expr.ShellCompile`) |
| `steps.<id>.outputs.<k>` | read from `/workspace/.easydev/out/<index>` by `easydev_out` |
| step outcome / exit code | written to a `##[easydev:result]` block at the END of the log |
| step env | exported inside the step's subshell, so it cannot leak forward |
| `continue-on-error` | reports failure without failing the job or tainting `success()` |

Step bodies are embedded **base64-encoded**: a workflow may contain any text, so
a literal heredoc delimiter (or a forged `##[easydev:result]` line) would be an
injection vector.

A step with no `if:` implicitly requires every previous step to have succeeded —
that is what makes `if: always()` meaningful.

### 4.2 Checkout

The supervisor script starts with

```sh
curl -fsSL -u "$EASYDEV_CHECKOUT_AUTH" \
  'http://<vcs>/api/v1/namespaces/<ns>/archive/<head>.tar.gz' | tar -xz -C .
```

- No git client, no clone cache.
- The tree is **byte-for-byte the pinned snapshot** the workflow was read from.
- The credential travels in an environment variable, never in the script.
- It runs **once per job**, before the first step sees the tree.

### 4.2 Untrusted code

Every CI job is submitted with `Untrusted: true`, which makes `jobmgr`:

- set `automountServiceAccountToken: false` (**without this, any tenant's
  workflow can call the Kubernetes API as the cluster identity**),
- run as non-root with `readOnlyRootFilesystem`,
- drop **all** capabilities and apply `RuntimeDefault` seccomp,
- refuse an explicit `service_account`,
- mount `/workspace` (emptyDir, or a PVC for cache) and `/tmp`.

### 4.3 Step outputs

A step writes `NAME=VALUE` lines to `$EASYDEV_OUTPUT` (a file). The supervisor
script prints them behind a `##[easydev:outputs]` marker after the step's own
output, so a program that prints the same text cannot forge an output. The
orchestrator parses marker → `steps.<id>.outputs` → `jobs.<id>.outputs` →
`needs.<job>.outputs`.

## 5. HTTP API

Tenant-scoped (the tenant in the path is authoritative):

| Method | Path | Notes |
|---|---|---|
| POST | `/api/v1/{ns}/ci/events` | start a run. **The body carries no YAML**: the workflow is read from the VCS |
| GET | `/api/v1/{ns}/ci/runs` | list runs |
| GET | `/api/v1/{ns}/ci/runs/{id}` | one run: job instances, steps, outputs, log tail |
| POST | `/api/v1/{ns}/ci/runs/{id}/cancel` | request cancellation |
| POST | `/api/v1/{ns}/ci/runs/{id}/rerun` | re-run the **same** pinned workflow snapshot |
| GET | `/api/v1/{ns}/ci/checks?rr=` | checks + `required_pending` |
| GET | `/api/v1/{ns}/ci/workflows?ref=` | the workflow files a ref declares |

Event body:

```jsonc
{ "event": "rr_opened",        // or rr_synced | rr_merged | tag_created | manual
  "ref": "main",               // the target branch
  "base": "<base snapshot>",   // workflow is read HERE for RR events
  "head": "<head snapshot>",   // checkout happens HERE
  "rr": 42,
  "changed_paths": ["src/a.go"],
  "workflow": ".easydev/workflows/ci.yml",  // optional
  "inputs": {"k": "v"} }
```

## 6. Merge gate

`POST /api/v1/namespaces/{ns}/rr/{iid}/merge` is wrapped by the gateway. While a
`required: true` check is not `success`, the merge is refused with **409** and
the failing checks, **before** easyvcs is consulted. easyvcs itself never learns
about CI.

`?allow_ungated=true` is the documented escape hatch for an operator override.

## 7. Secrets and permissions

Both use the same rule: **a credential is never interpolated; it is referenced.**

### Secrets

```yaml
jobs:
  deploy:
    env:
      TOKEN: ${{ secrets.deploy }}
    steps:
      - run: ./deploy --token "$TOKEN"
```

- `${{ secrets.deploy }}` resolves to a shell variable reference
  (`$EASYDEV_SECRET_DEPLOY_TOKEN`), not to the value.
- The value is written to a **per-run Secret** (`ci-<run-id>`), removed when the
  run ends, and injected by the kubelet through a `secretKeyRef`.
- A reference to a secret the tenant does not have **fails the run** at plan
  time: silently substituting `""` would deploy with an empty credential.

### Permissions

```yaml
permissions:
  contents: read     # the VCS: read the snapshot
  packages: write    # the package registry
```

- The default is **none**: an undeclared workflow gets no credential.
- `contents: read` mints a read-only VCS token; `contents: write` mints a write
  token and exposes both `EASYDEV_VCS_TOKEN` and `EASYDEV_VCS_WRITE_TOKEN`.
- An unknown scope (e.g. `security-events`) or an invalid level is a hard error,
  and a requested scope the platform cannot grant **fails the run** instead of
  running with less than the workflow believes it has.
- Tokens are minted per run from the VCS store (one token authority), so one
  run's credential can be revoked without touching another's.

## 8. Cancellation and live logs

- **Cancel interrupts a running job.** The orchestrator owns the job's context, a
  watcher cancels it on a cancellation request, and the executor job is DELETED —
  which is what actually kills the container. (Cancelling only at step boundaries
  meant a 30-minute job ignored a cancel for 30 minutes.)
- **Live logs**: `GET /api/v1/{ns}/ci/runs/{id}/logs/{instanceKey}` streams
  chunks as the job produces them and closes when the run is terminal. Without
  it, a long step was silent until it finished.

## 9. Deliberate non-goals

| Not doing | Why |
|---|---|
| a long-lived runner service as the default | a k8s Job already is the executor; a service adds operations for no isolation gain |
| a JS/Docker action runtime | composite actions cover the cases with a fraction of the attack surface |
| a marketplace / global action registry | `namespace/path@ref` + a subtree archive is enough, and tags are immutable |
| anonymous workflow execution | a workflow always comes from a snapshot with a resolved identity |
| `pull_request_target`-style dual semantics | one `on.rr`; base rules, head code |

## 8. Status / roadmap

Implemented and covered by tests (`internal/ci`, `internal/wf`,
`internal/platformtest`):

| Area | State |
|---|---|
| parsing, v1 compatibility, matrix (include/exclude), `if:`, `needs`, outputs | done |
| expression language (`internal/ci/expr`) | done |
| `uses:` resolution to a subtree at a pinned ref; shell + composite actions | done |
| one executor job per instance: steps share `/workspace`, checkout once | done |
| runtime `if:` gates, per-step env isolation, `continue-on-error` | done |
| per-step execution with a pinned-snapshot checkout | done |
| step → job output propagation (`$EASYDEV_OUTPUT` marker) | done |
| checks + merge gate (409 before easyvcs sees the merge) | done |
| cancel, re-run (re-pinned), workflow listing | done |
| durable state (`ci.SQLStore`: sqlite/pg/mysql, pure-Go driver) | done |
| `concurrency:` group + cancel-in-progress (cross-replica) | done |
| `strategy.max-parallel` / `fail-fast` honoured | done |
| cancellation interrupts a RUNNING job (not just between steps) | done |
| live log streaming (`GET .../runs/{id}/logs/{instance}`) | done |
| `secrets:` as Secret REFERENCES (value never in the spec) | done |
| `permissions:` enforced as a scoped, short-lived token | done |
| `Untrusted` job hardening (no SA token, non-root, RO rootfs, no caps) | done |
| end-to-end test: VCS snapshot → workflow → run → check | done |

Next:

1. **Log archival to easyblobs** — the full step log is currently truncated to
   the inline tail; the `LogSHA` field is reserved for it.
2. **Cache PVCs** keyed by `(tenant, lockfile hash)` — `CreateJob` already accepts
   a workspace claim, so this is wiring.
3. **Event source** — an easyvcs `AuditSink` (the `rr-merge` / `commit` events
   are already recorded there) or a signed webhook, so runs start without a
   manual `POST /ci/events`.
4. **Per-step images inside one Pod** — today a job runs in ONE image (the job's
   container). Supporting `uses: docker://...` or a step-level `container:`
   needs Tekton's entrypoint-rewrite pattern (one container per step in the same
   Pod, sequenced by a supervisor). The current single-script supervisor is
   deliberately simpler: it covers shell/composite actions today.
5. **Optional self-hosted runner** — a fourth `OpsClient`, for execution outside
   the cluster.
