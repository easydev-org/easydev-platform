package ci

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/easydev-org/easydev-platform/internal/ci/expr"
)

// Event is what easyvcs reports when something happened that may start a
// workflow. It carries the PINNED snapshots, never a mutable branch pointer.
type Event struct {
	Namespace string    `json:"namespace"`
	Event     EventType `json:"event"`
	Trigger   string    `json:"trigger,omitempty"` // legacy alias for Event

	// Ref is the branch the event applies to; Tag is set for tag events.
	Ref string `json:"ref,omitempty"`
	Tag string `json:"tag,omitempty"`

	// Base/Head are the pinned snapshot shas (base = merge base / target tip).
	Base string `json:"base,omitempty"`
	Head string `json:"head,omitempty"`

	// RR is the Revision Request this event belongs to (0 when none).
	RR int64 `json:"rr,omitempty"`

	// ChangedPaths is the set of paths the change touched (for `on.rr.paths`).
	ChangedPaths []string `json:"changed_paths,omitempty"`

	// Owner is the authenticated caller; Actor is who authored the change.
	Owner string `json:"owner,omitempty"`
	Actor string `json:"actor,omitempty"`

	// WorkflowPath is the file to read (default DefaultWorkflowPath), and
	// WorkflowSHA is the snapshot it must be read from. They are resolved by
	// the caller (the gateway) so a workflow can never come from the request
	// body.
	WorkflowPath string `json:"workflow_path,omitempty"`
	WorkflowSHA  string `json:"workflow_sha,omitempty"`

	// Inputs are workflow_dispatch inputs.
	Inputs map[string]string `json:"inputs,omitempty"`

	// Workflow is the ALREADY PARSED workflow. The gateway resolves it from the
	// VCS; the orchestrator never accepts YAML from an HTTP body.
	Workflow *Workflow `json:"-"`
}

// DefaultWorkflowPath is where a namespace's primary workflow lives.
const DefaultWorkflowPath = ".easydev/workflows/ci.yml"

// WorkflowDir is the directory scanned for additional workflows.
const WorkflowDir = ".easydev/workflows"

// NormalizedEvent returns the event kind, honouring the legacy Trigger field.
func (e *Event) NormalizedEvent() EventType {
	if e.Event != "" {
		return e.Event
	}
	if e.Trigger != "" {
		return EventType(e.Trigger)
	}
	return ""
}

// TriggerOf maps an event kind to the trigger recorded on a run.
func TriggerOf(ev EventType) Trigger {
	switch ev {
	case EventRROpened:
		return TriggerRROpened
	case EventRRSynced:
		return TriggerRRSynced
	case EventRRMerged:
		return TriggerRRMerged
	case EventTag:
		return TriggerTag
	case EventSchedule:
		return TriggerSchedule
	}
	return TriggerManual
}

// Orchestrator runs workflows. It is stateless apart from its Store: every
// decision is a function of (event, workflow, store, ops).
type Orchestrator struct {
	store Store
	ops   OpsClient
	// MaxParallel caps concurrently running job instances within a stage.
	MaxParallel int
	// now is injectable for tests.
	now func() time.Time
	// opsFor, when set, returns a PER-RUN OpsClient bound to the run's tenant.
	opsFor func(tenant string) OpsClient
	// logCap bounds the inline log tail stored on a result.
	logCap int
	// gate enforces `concurrency:` when the workflow declares it (nil = off).
	gate *ConcurrencyGate
	// mu guards seq/execSeq (id uniqueness within one process).
	mu  sync.Mutex
	seq uint64
	// execSeq numbers executor jobs so their names are unique even after
	// sanitize's 60-char truncation.
	execSeq uint64
	// vcsBase/vcsUser/vcsToken let the supervisor script fetch the checkout.
	// They are set by WithVCS.
	vcsBase  string
	vcsUser  string
	vcsToken string
	// logMu guards logSubs (live log subscribers, one per run instance).
	logMu   sync.Mutex
	logSubs map[string]chan string
	// onLog, when set, observes every forwarded log chunk (tests, and a future
	// websocket fan-out). Normal subscribers go through SubscribeLogs.
	onLog func(runID, instanceKey, chunk string)
	// artifactsBase is the package-registry base URL; empty disables `artifacts:`.
	artifactsBase string
	// tokens mints the scoped credential `permissions:` grants. nil = a workflow
	// that requests access fails at plan time.
	tokens TokenMinter
	// secrets resolves a tenant's stored secrets. nil = secrets are disabled.
	secrets SecretsProvider
	// secretWriter persists the per-run Secret the jobs reference. nil = the
	// backend resolves references itself (nothing to create).
	secretWriter SecretWriter
}

// WithSecrets enables `${{ secrets.X }}`. Both a provider (to read the values)
// and a writer (to create the per-run Secret the jobs reference) are needed: the
// value must reach the kubelet, not the Job spec.
func (o *Orchestrator) WithSecrets(p SecretsProvider, w SecretWriter) *Orchestrator {
	o.secrets, o.secretWriter = p, w
	return o
}

// WithTokenMinter enables `permissions:` enforcement.
func (o *Orchestrator) WithTokenMinter(m TokenMinter) *Orchestrator {
	o.tokens = m
	return o
}

// SecretWriter persists the ephemeral Secret a run's jobs reference.
type SecretWriter interface {
	// PutRunSecret creates or replaces the run's Secret with the given values.
	PutRunSecret(ctx context.Context, tenant, name string, values map[string]string) error
	// DeleteRunSecret removes it after the run.
	DeleteRunSecret(ctx context.Context, tenant, name string) error
}

// WithVCS configures how the in-container supervisor fetches the source tree.
// Empty base means "no checkout" (a job that does not need the repo).
func (o *Orchestrator) WithVCS(base, user, token string) *Orchestrator {
	o.vcsBase, o.vcsUser, o.vcsToken = base, user, token
	return o
}

// WithConcurrency enables `concurrency:` enforcement.
func (o *Orchestrator) WithConcurrency(g *ConcurrencyGate) *Orchestrator {
	o.gate = g
	return o
}

// NewOrchestrator wires an orchestrator.
func NewOrchestrator(store Store, ops OpsClient) *Orchestrator {
	return &Orchestrator{store: store, ops: ops, MaxParallel: 4, now: time.Now, logCap: 4096}
}

// WithPerTenantOps binds the orchestrator to a FACTORY: every run uses a client
// scoped to its own tenant (namespace). nil restores the single-client mode.
func (o *Orchestrator) WithPerTenantOps(fn func(tenant string) OpsClient) *Orchestrator {
	o.opsFor = fn
	return o
}

// WithLogCap sets how many log bytes are kept inline (0 = default 4096).
func (o *Orchestrator) WithLogCap(n int) *Orchestrator {
	if n > 0 {
		o.logCap = n
	}
	return o
}

func (o *Orchestrator) opsForRun(ns string) OpsClient {
	if o.opsFor != nil {
		if c := o.opsFor(ns); c != nil {
			return c
		}
	}
	return o.ops
}

// ErrNotTriggered means the workflow does not subscribe to the event. It is not
// an error for the caller: nothing ran.
var ErrNotTriggered = fmt.Errorf("ci: workflow does not trigger on this event")

// Run starts a workflow for an event and returns the created run id. The stages
// execute asynchronously; the caller polls the store.
func (o *Orchestrator) Run(ctx context.Context, ev Event) (string, error) {
	wf := ev.Workflow
	if wf == nil {
		return "", fmt.Errorf("ci: no workflow resolved for the event (the gateway must read it from the VCS)")
	}
	kind := ev.NormalizedEvent()
	if kind == "" {
		return "", fmt.Errorf("ci: event is required")
	}
	if !wf.TriggeredBy(&ev) {
		return "", ErrNotTriggered
	}
	plan, err := wf.Plan()
	if err != nil {
		return "", err
	}

	now := o.now().UTC().UnixMilli()

	run := &Run{
		ID:           o.newRunID(now, ev.Namespace, firstNonEmpty(ev.Ref, ev.Tag, "manual")),
		Namespace:    ev.Namespace,
		Workflow:     wf.Name,
		WorkflowPath: ev.WorkflowPath,
		WorkflowSHA:  ev.WorkflowSHA,
		Trigger:      TriggerOf(kind),
		Ref:          ev.Ref,
		Base:         ev.Base,
		Head:         ev.Head,
		Tag:          ev.Tag,
		ChangedPaths: ev.ChangedPaths,
		RR:           ev.RR,
		State:        RunRunning,
		Created:      now,
		Owner:        ev.Owner,
		Actor:        ev.Actor,
	}
	// `concurrency:` — at most one live run per group.
	if wf.Concurrency != nil && o.gate != nil {
		group := GroupOf(wf.Concurrency.Group, run)
		run.Group = group
		if _, err := o.gate.Acquire(ctx, group, run, wf.Concurrency.CancelInProgress); err != nil {
			return "", err
		}
	}
	for _, stage := range plan.Stages {
		for _, inst := range stage {
			run.Jobs = append(run.Jobs, JobResult{
				Key: inst.Key, JobID: inst.Job.ID, Matrix: inst.Matrix,
				State: RunPending,
			})
			run.Steps += len(inst.Job.Steps)
		}
	}
	// Secrets are resolved BEFORE the run exists: a workflow that references a
	// secret the tenant does not have must fail here, never start a deploy with an
	// empty credential.
	secretEnv, secretRefs, secretCtx, serr := o.resolveSecrets(ctx, run, wf)
	if serr != nil {
		return "", serr
	}
	// `permissions:` decides which credential the job gets; a request the platform
	// cannot satisfy fails here for the same reason.
	permEnv, perr := o.resolvePermissions(ctx, run, wf)
	if perr != nil {
		return "", perr
	}
	// Permission tokens join the same per-run secret, so they are referenced
	// rather than written into the job spec.
	for k, v := range permEnv {
		if secretEnv == nil {
			secretEnv = map[string]string{}
		}
		secretEnv[k] = v
		secretRefs = append(secretRefs, SecretEnvVar{
			Env: k, Secret: runSecretName(run), Key: sanitize(k),
		})
	}
	if len(secretEnv) > 0 {
		if o.secretWriter != nil {
			// The values must reach the kubelet as a SECRET, not the Job spec.
			if werr := o.secretWriter.PutRunSecret(ctx, ev.Namespace, runSecretName(run), secretEnv); werr != nil {
				return "", fmt.Errorf("ci: store run secrets: %w", werr)
			}
			run.secretOwned = true
		}
		run.secrets, run.secretRefs, run.secretCtx = secretEnv, secretRefs, secretCtx
	}

	if err := o.store.PutRun(ctx, run); err != nil {
		return "", err
	}
	_ = o.store.PutCheck(ctx, &Check{
		RR: ev.RR, Namespace: ev.Namespace, Name: wf.Name,
		State: RunRunning, Required: plan.Required, RunID: run.ID, Details: "running",
	})
	go o.execute(context.Background(), run, plan, o.opsForRun(ev.Namespace), wf)
	return run.ID, nil
}

func (o *Orchestrator) execute(ctx context.Context, run *Run, plan *Plan, ops OpsClient, wf *Workflow) {
	byKey := map[string]*JobResult{}
	for i := range run.Jobs {
		byKey[run.Jobs[i].Key] = &run.Jobs[i]
	}
	failed := false
	for _, stage := range plan.Stages {
		if failed {
			for _, inst := range stage {
				if r, ok := byKey[inst.Key]; ok && r.State == RunPending {
					r.State = RunCanceled
				}
			}
			continue
		}
		// The stage's concurrency is the SMALLEST `max-parallel` any instance in
		// it declares (the workflow's own limit wins over the server default).
		sem := make(chan struct{}, stageParallelism(stage, o.MaxParallel))
		var wg sync.WaitGroup
		var mu sync.Mutex
		for _, inst := range stage {
			// Skip an instance whose `needs` did not succeed.
			if !depsOK(inst, byKey) {
				if r, ok := byKey[inst.Key]; ok {
					r.State = RunSkipped
				}
				continue
			}
			wg.Add(1)
			go func(inst *Instance) {
				defer wg.Done()
				// Acquire a slot FIRST, then re-check: the abort flag can only
				// change while another instance holds/was holding a slot, so
				// checking before the semaphore would race with the failure that
				// sets it (and could stop an instance that should have run).
				sem <- struct{}{}
				defer func() { <-sem }()

				// fail-fast: once a sibling has failed, do not START the rest.
				mu.Lock()
				abort := failed && failFast(stage)
				mu.Unlock()
				if abort {
					if r, ok := byKey[inst.Key]; ok && r.State == RunPending {
						r.State = RunCanceled
					}
					return
				}

				started := o.now().UTC().UnixMilli()
				res := o.runInstance(ctx, run, inst, wf, byKey, ops)
				finished := o.now().UTC().UnixMilli()

				mu.Lock()
				defer mu.Unlock()
				r := byKey[inst.Key]
				r.Started, r.Finished = started, finished
				r.State, r.ExitCode, r.Error = res.state, res.exit, res.err
				r.Steps, r.LogsTail, r.LogSHA = res.steps, res.tail, res.logSHA
				r.Outputs = res.outputs
				if r.State != RunSuccess && !inst.Job.ContinueOnError {
					failed = true
				}
			}(inst)
		}
		wg.Wait()
		_ = o.store.PutRun(ctx, run)
	}

	if failed {
		run.State = RunFailed
	} else {
		run.State = RunSuccess
	}
	_ = o.store.PutRun(ctx, run)
	details := "all jobs succeeded"
	if failed {
		details = "one or more jobs failed"
	}
	// The per-run Secret holds the tenant's credentials for exactly as long as
	// the run: remove it now that nothing can reference it.
	if run.secretOwned && o.secretWriter != nil {
		if derr := o.secretWriter.DeleteRunSecret(context.Background(), run.Namespace, runSecretName(run)); derr != nil {
			log.Printf("ci: delete run secret %s: %v", runSecretName(run), derr)
		}
	}
	failing := ""
	for _, j := range run.Jobs {
		if j.State == RunFailed && failing == "" {
			failing = j.Key
		}
	}
	_ = o.store.PutCheck(ctx, &Check{
		RR: run.RR, Namespace: run.Namespace, Name: wf.Name,
		State: run.State, Required: plan.Required, RunID: run.ID,
		Details: details, Job: failing,
	})
}

type instanceResult struct {
	state   RunState
	exit    int
	steps   []StepResult
	outputs map[string]string
	tail    string
	logSHA  string
	err     string
}

// runInstance executes one job instance: resolve `if:`, run its steps in order,
// and stop at the first failing step (unless it is continue-on-error).
// newRunID builds a unique run id.
//
// The timestamp is not enough on its own: two runs created in the same
// millisecond (a re-run, or two events delivered together) would collide and the
// store would silently overwrite one. A monotonic counter keeps ids unique and
// ordered within the process; across replicas the nanosecond suffix plus the
// counter is enough in practice (an id is a handle, not a security boundary).
func (o *Orchestrator) newRunID(now int64, ns, slug string) string {
	o.mu.Lock()
	o.seq++
	seq := o.seq
	o.mu.Unlock()
	return fmt.Sprintf("run-%d-%d-%s", now, seq, sanitize(ns+"-"+slug))
}

// baseVars builds the expression variable tree for one job instance.
//
// `needs` exposes the OUTPUTS of instances that already finished (always in an
// earlier stage, so the map is immutable while this instance runs).
func (o *Orchestrator) baseVars(run *Run, inst *Instance, wf *Workflow, results map[string]*JobResult) map[string]any {
	head := run.Head
	needs := map[string]any{}
	for _, need := range inst.Needs {
		if r, ok := results[need]; ok {
			v := map[string]any{
				"result":  string(r.State),
				"outputs": toAnyMapStrings(r.Outputs),
			}
			// Both spellings resolve: the instance key, and the job id (a
			// matrix-less `needs: [meta]` reads `needs.meta`).
			needs[need] = v
			if r.JobID != "" {
				needs[r.JobID] = v
			}
		}
	}
	return map[string]any{
		// `easyvcs` is the native context: the VCS is the platform's identity
		// source, so its name leads.
		"easyvcs": map[string]any{
			"namespace": run.Namespace, "rr": float64(run.RR), "ref": run.Ref,
			"head": head, "base": run.Base, "tag": run.Tag,
			"trigger": string(run.Trigger), "actor": run.Actor, "owner": run.Owner,
			"paths": toAnyList(run.ChangedPaths),
		},
		// `github` and `env` aliases keep GitHub workflows working unchanged.
		"github": map[string]any{
			"repository": run.Namespace, "ref": run.Ref, "sha": head,
			"event_name": string(run.Trigger), "actor": run.Actor,
			"workflow": wf.Name,
		},
		"job":      map[string]any{"id": inst.Job.ID, "key": inst.Key},
		"matrix":   toAnyMap(inst.Matrix),
		"env":      toStringAnyMap(wf.Env),
		"secrets":  map[string]any{},
		"runner":   map[string]any{"os": runnerOS(inst.Job)},
		"workflow": map[string]any{"name": wf.Name, "path": run.WorkflowPath, "sha": run.WorkflowSHA},
		"needs":    needs,
		"steps":    map[string]any{},
	}
}

// canceled reports whether the run was asked to stop.
//
// It asks the store first (a SQLStore sees a cancellation issued by ANOTHER
// replica), then falls back to the run's own state.
func (o *Orchestrator) canceled(ctx context.Context, runID string) bool {
	if c, ok := o.store.(Cancelable); ok {
		return c.IsCanceled(ctx, runID)
	}
	if s, ok := o.store.(*MemStore); ok {
		return s.isCanceled(runID)
	}
	r, err := o.store.GetRun(ctx, runID)
	return err == nil && r.State == RunCanceled
}

func depsOK(inst *Instance, results map[string]*JobResult) bool {
	for _, need := range inst.Needs {
		r, ok := results[need]
		if !ok {
			return false
		}
		if r.State != RunSuccess && r.State != RunSkipped {
			return false
		}
	}
	return true
}

func imageFor(j *WorkflowJob) string {
	if j.Container != nil && j.Container.Image != "" {
		return j.Container.Image
	}
	return "ubuntu:24.04"
}

func serviceSpecs(j *WorkflowJob) map[string]ServiceSpec {
	if len(j.Services) == 0 {
		return nil
	}
	out := make(map[string]ServiceSpec, len(j.Services))
	for name, s := range j.Services {
		out[name] = ServiceSpec{
			Image: s.Image, Env: s.Env, Ports: s.Ports,
			Command: s.Command, Args: s.Args,
		}
	}
	return out
}

func stepTimeout(j *WorkflowJob, st *Step) int {
	if st.TimeoutMinutes > 0 {
		return st.TimeoutMinutes * 60
	}
	if j.TimeoutMinutes > 0 {
		// The per-step budget is the whole job's; the executor enforces the run.
		return j.TimeoutMinutes * 60
	}
	return 0
}

func checkoutSpec(run *Run) *CheckoutSpec {
	if run.Head == "" {
		return nil
	}
	return &CheckoutSpec{Ref: run.Head, Path: "."}
}

func runnerOS(j *WorkflowJob) string {
	for _, l := range j.RunsOn {
		switch strings.ToLower(l) {
		case "windows", "macos", "linux":
			return strings.ToLower(l)
		}
	}
	return "linux"
}

func mergeEnv(base, over map[string]string) map[string]string {
	if len(base) == 0 && len(over) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

func interpolateMap(m map[string]string, ctx *expr.Ctx) (map[string]string, error) {
	if len(m) == 0 {
		return map[string]string{}, nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		s, err := expr.Interpolate(v, ctx)
		if err != nil {
			return nil, fmt.Errorf("env %s: %w", k, err)
		}
		out[k] = s
	}
	return out, nil
}

// parseOutputs extracts `NAME=VALUE` lines written to $EASYDEV_OUTPUT (the path
// the executor exports) from the step's log tail. Marker lines are emitted by
// the supervisor script, not by the user's program, so a program that prints the
// same shape cannot forge an output.
func parseOutputs(jobName, logs string) map[string]string {
	const begin = "##[easydev:outputs]"
	i := strings.LastIndex(logs, begin)
	if i < 0 {
		return nil
	}
	out := map[string]string{}
	for _, line := range strings.Split(logs[i+len(begin):], "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "##[easydev:") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || k == "" {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func joinTail(prev, next string, capBytes int) string {
	all := prev + next
	if capBytes > 0 && len(all) > capBytes {
		all = all[len(all)-capBytes:]
	}
	return all
}

func toAnyList(in []string) []any {
	out := make([]any, 0, len(in))
	for _, s := range in {
		out = append(out, s)
	}
	return out
}

func toAnyMap(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func toStringAnyMap(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func toAnyMapStrings(in map[string]string) map[string]any { return toStringAnyMap(in) }

func firstNonEmpty(list ...string) string {
	for _, s := range list {
		if s != "" {
			return s
		}
	}
	return ""
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// SortedJobKeys returns the run's job keys in a stable order (API helpers).
func SortedJobKeys(r *Run) []string {
	out := make([]string, 0, len(r.Jobs))
	for _, j := range r.Jobs {
		out = append(out, j.Key)
	}
	sort.Strings(out)
	return out
}

// emitLog publishes a live log chunk.
//
// A subscriber sees the chunk as it is produced, so a long step is not silent
// until it ends. Subscribers are registered per (run, instance) by the HTTP layer;
// a chunk with no subscriber is dropped (a run may be created by an API caller
// that never opened the stream).
func (o *Orchestrator) emitLog(runID, instanceKey, chunk string) {
	if chunk == "" {
		return
	}
	if o.onLog != nil {
		o.onLog(runID, instanceKey, chunk)
	}
	o.logMu.Lock()
	sub := o.logSubs[runID+"\x00"+instanceKey]
	o.logMu.Unlock()
	if sub == nil {
		return
	}
	select {
	case sub <- chunk:
	default:
		// A slow subscriber must never block a run: drop rather than stall. The
		// final log is available from the store, so nothing is lost permanently.
	}
}

// SubscribeLogs registers a channel that receives a run instance's live log.
// The returned function unsubscribes; the channel is closed on unsubscribe.
func (o *Orchestrator) SubscribeLogs(runID, instanceKey string) (<-chan string, func()) {
	ch := make(chan string, 256)
	key := runID + "\x00" + instanceKey
	o.logMu.Lock()
	if o.logSubs == nil {
		o.logSubs = map[string]chan string{}
	}
	o.logSubs[key] = ch
	o.logMu.Unlock()
	return ch, func() {
		o.logMu.Lock()
		if o.logSubs[key] == ch {
			delete(o.logSubs, key)
			close(ch)
		}
		o.logMu.Unlock()
	}
}

// stageParallelism is the concurrency for one stage.
//
// `strategy.max-parallel` was parsed but never read, so a job that declared it
// ran as wide as the server default. The stage uses the SMALLEST declared limit
// (a limit is a "no more than", and several instances sharing a stage must all
// respect theirs); zero means "no instance declared one", so the server default
// applies.
func stageParallelism(stage []*Instance, serverDefault int) int {
	limit := 0
	for _, inst := range stage {
		if m := inst.Job.Strategy; m != nil && m.MaxParallel > 0 {
			if limit == 0 || m.MaxParallel < limit {
				limit = m.MaxParallel
			}
		}
	}
	if limit == 0 {
		return maxInt(serverDefault, 1)
	}
	return limit
}

// failFast reports whether a failure in this stage stops the rest of it.
//
// A stage is fail-fast unless an instance explicitly set `fail-fast: false`
// (GitHub's default is true for a matrix).
func failFast(stage []*Instance) bool {
	for _, inst := range stage {
		if m := inst.Job.Strategy; m != nil && !m.FailFast {
			return false
		}
	}
	return true
}
