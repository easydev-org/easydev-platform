package ci

import (
	"context"
	"fmt"
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
		ID:           fmt.Sprintf("run-%d-%s", now, sanitize(ev.Namespace+"-"+firstNonEmpty(ev.Ref, ev.Tag, "manual"))),
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
	for _, stage := range plan.Stages {
		for _, inst := range stage {
			run.Jobs = append(run.Jobs, JobResult{
				Key: inst.Key, JobID: inst.Job.ID, Matrix: inst.Matrix,
				State: RunPending,
			})
			run.Steps += len(inst.Job.Steps)
		}
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
		sem := make(chan struct{}, maxInt(o.MaxParallel, 1))
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
				sem <- struct{}{}
				defer func() { <-sem }()

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
func (o *Orchestrator) runInstance(ctx context.Context, run *Run, inst *Instance, wf *Workflow, results map[string]*JobResult, ops OpsClient) instanceResult {
	j := inst.Job
	vars := o.baseVars(run, inst, wf, results)
	xctx := &expr.Ctx{Vars: vars, Status: expr.StatusSuccess}

	// `if:` on the job.
	if j.If != "" {
		ok, err := expr.EvalBool(j.If, xctx)
		if err != nil {
			return instanceResult{state: RunFailed, err: "if: " + err.Error()}
		}
		if !ok {
			return instanceResult{state: RunSkipped}
		}
	}

	out := instanceResult{state: RunRunning}
	// stepsCtx accumulates `steps.<id>.outputs.<k>` as steps finish, so a job's
	// declared `outputs:` can be resolved at the end.
	stepsCtx := map[string]any{}
	vars["steps"] = stepsCtx

	for i := range j.Steps {
		st := &j.Steps[i]
		if o.canceled(ctx, run.ID) {
			out.state = RunCanceled
			return out
		}
		sctx := &expr.Ctx{Vars: vars, Status: expr.StatusSuccess}
		if st.If != "" {
			ok, err := expr.EvalBool(st.If, sctx)
			if err != nil {
				out.steps = append(out.steps, StepResult{ID: st.ID, Name: st.Name, State: RunFailed, Error: "if: " + err.Error()})
				if !st.ContinueOnError {
					out.state, out.exit = RunFailed, 1
					return out
				}
				out.steps = append(out.steps, StepResult{ID: st.ID, Name: st.Name, State: RunSkipped})
				continue
			}
			if !ok {
				out.steps = append(out.steps, StepResult{ID: st.ID, Name: st.Name, State: RunSkipped})
				continue
			}
		}

		sr, logTail := o.runStep(ctx, run, inst, st, vars, xctx, ops)
		out.steps = append(out.steps, sr)
		if sr.ID != "" {
			stepsCtx[sr.ID] = map[string]any{
				"outputs": toAnyMapStrings(sr.Outputs),
				"outcome": string(sr.State),
			}
		}
		if logTail != "" {
			out.tail = joinTail(out.tail, logTail, o.logCap)
		}
		if sr.State == RunFailed {
			if st.ContinueOnError {
				xctx.Status = expr.StatusFailure
				continue
			}
			out.state, out.exit = RunFailed, sr.ExitCode
			if out.exit == 0 {
				out.exit = 1
			}
			return out
		}
		// A step that failed but continued taints success() for later steps.
		if sr.State != RunSuccess {
			xctx.Status = expr.StatusFailure
		}
	}
	out.state = RunSuccess
	// Resolve the job's declared outputs (`jobs.<id>.outputs`).
	if len(j.Outputs) > 0 {
		octx := &expr.Ctx{Vars: vars, Status: expr.StatusSuccess}
		out.outputs = map[string]string{}
		for name, tmpl := range j.Outputs {
			s, err := expr.Interpolate(tmpl, octx)
			if err != nil {
				continue // a bad output expression must not fail a green job
			}
			out.outputs[name] = s
		}
	}
	return out
}

// runStep runs ONE step. A `uses:` action is resolved to its shell/composite
// form by the caller-provided ActionResolver (see actions.go); a `run:` step is
// a shell script. Both end up as commands on the executor.
func (o *Orchestrator) runStep(ctx context.Context, run *Run, inst *Instance, st *Step, vars map[string]any, xctx *expr.Ctx, ops OpsClient) (StepResult, string) {
	res := StepResult{ID: st.ID, Name: st.Name}

	// Interpolate the environment and the script. Everything a workflow can
	// inject into the shell goes through ENV or is shell-quoted; interpolated
	// values are NEVER concatenated raw into `run:` (that would be an
	// injection vector).
	env, err := interpolateMap(mergeEnv(inst.Job.Env, st.Env), xctx)
	if err != nil {
		res.State, res.Error = RunFailed, err.Error()
		return res, ""
	}

	var script string
	if st.Uses != "" {
		// The action's definition is a shell script (or a composite of steps)
		// resolved from the VCS and pinned by ref — see actions.go.
		a, aerr := ResolveAction(st.Uses, inst.Job, st)
		if aerr != nil {
			res.State, res.Error = RunFailed, aerr.Error()
			return res, ""
		}
		script = a.Script
		for k, v := range a.Env {
			env[k] = v
		}
	} else {
		s, ierr := expr.Interpolate(st.Run, xctx)
		if ierr != nil {
			res.State, res.Error = RunFailed, ierr.Error()
			return res, ""
		}
		script = s
	}

	shell := st.Shell
	if shell == "" {
		shell = "bash"
	}
	workdir := st.WorkingDirectory

	spec := JobSpec{
		Name:           sanitize(fmt.Sprintf("%s-%s-%s-%d", run.ID, inst.Key, run.Head, len(res.ID))),
		Namespace:      run.Namespace,
		Owner:          run.Owner,
		Image:          imageFor(inst.Job),
		Commands:       []string{script},
		Env:            env,
		Shell:          shell,
		WorkDir:        workdir,
		CPU:            "",
		Memory:         "",
		TimeoutSeconds: stepTimeout(inst.Job, st),
		Checkout:       checkoutSpec(run),
		// Every step is one container (or one script) — the executor decides.
		Services: serviceSpecs(inst.Job),
	}

	jobID, err := ops.RunJob(ctx, spec)
	if err != nil {
		res.State, res.Error = RunFailed, err.Error()
		return res, ""
	}
	code, werr := ops.WaitJob(ctx, jobID)
	logs, _ := ops.Logs(ctx, jobID)
	_ = ops.DeleteJob(ctx, jobID)

	res.ExitCode = code
	switch {
	case werr != nil && strings.Contains(werr.Error(), "cancel"):
		res.State = RunCanceled
	case code == 0:
		res.State = RunSuccess
	default:
		res.State = RunFailed
		if werr != nil {
			res.Error = werr.Error()
		}
	}
	res.Outputs = parseOutputs(spec.Name, logs)
	return res, logs
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
func (o *Orchestrator) canceled(ctx context.Context, runID string) bool {
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
