package ci

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/easydev-org/easydev-platform/internal/ci/expr"
)

// runInstance executes ONE job instance.
//
// It submits exactly ONE executor job: every step of the job runs sequentially
// inside that one container, so they share /workspace (a compile in step 1 is
// visible to a test in step 2), the checkout happens once, and there is no way
// for two steps to collide on an executor name.
//
// See supervisor.go for the script and the runtime-evaluation trade-off.
func (o *Orchestrator) runInstance(ctx context.Context, run *Run, inst *Instance, wf *Workflow, results map[string]*JobResult, ops OpsClient) instanceResult {
	j := inst.Job
	vars := o.baseVars(run, inst, wf, results)

	// Secrets are resolved ONCE per run into a per-run Secret object; the job spec
	// carries only REFERENCES into it, so a value never enters the script, the
	// spec, or this process's long-lived state.
	secretRefs, secretCtx := run.secretRefs, run.secretCtx
	if secretCtx != nil {
		vars["secrets"] = secretCtx
	}

	// `if:` on the JOB is evaluated here: no step has run yet, so there is no
	// runtime state to consider.
	if j.If != "" {
		xctx := &expr.Ctx{Vars: vars, Status: expr.StatusSuccess}
		ok, err := expr.EvalBool(j.If, xctx)
		if err != nil {
			return instanceResult{state: RunFailed, err: "if: " + err.Error()}
		}
		if !ok {
			return instanceResult{state: RunSkipped}
		}
	}

	// Step ids are addressable by index inside the supervisor script.
	stepIndex := map[string]int{}
	for i := range j.Steps {
		if id := strings.TrimSpace(j.Steps[i].ID); id != "" {
			stepIndex[id] = i
		}
	}

	plan := make([]stepPlan, 0, len(j.Steps))
	baseEnv, err := interpolateMap(mergeEnv(wf.Env, j.Env), &expr.Ctx{Vars: vars})
	if err != nil {
		return instanceResult{state: RunFailed, err: err.Error()}
	}

	// The whole-job expression context. `steps.*.outputs.*` is NOT known here (it
	// is produced in the container), which is exactly why step-level `if:` and
	// the step bodies are compiled with a runtime-aware path below.
	for i := range j.Steps {
		st := &j.Steps[i]
		gate, gerr := compileGate(st.If, &expr.Ctx{Vars: vars}, stepIndex)
		if gerr != nil {
			return instanceResult{state: RunFailed, err: fmt.Sprintf("step %d if: %v", i+1, gerr)}
		}
		body, berr := o.stepBody(inst, st, vars)
		if berr != nil {
			return instanceResult{state: RunFailed, err: fmt.Sprintf("step %d: %v", i+1, berr)}
		}
		stepEnv, eerr := interpolateMap(st.Env, &expr.Ctx{Vars: vars})
		if eerr != nil {
			return instanceResult{state: RunFailed, err: fmt.Sprintf("step %d env: %v", i+1, eerr)}
		}
		merged := map[string]string{}
		for k, v := range baseEnv {
			merged[k] = v
		}
		for k, v := range stepEnv {
			merged[k] = v
		}
		plan = append(plan, stepPlan{
			index: i, id: st.ID, name: stepName(st, i),
			body: body, shell: st.Shell, workdir: st.WorkingDirectory,
			env: merged, gate: gate, continueOnErr: st.ContinueOnError,
		})
	}

	// Artifacts are declared per job and uploaded from INSIDE the container, so a
	// build product never travels through the control plane.
	targets, aerr := resolveArtifacts(j.Artifacts)
	if aerr != nil {
		return instanceResult{state: RunFailed, err: aerr.Error()}
	}
	artScript := ""
	if len(targets) > 0 && o.artifactBase() != "" {
		artScript = artifactScript(j, run, inst, targets)
	}

	script := supervisorScript(plan, checkoutSpec(run), o.vcsUser, o.vcsToken, o.vcsBase, run.Namespace, artScript)

	spec := JobSpec{
		// ONE executor job per instance. The name carries the instance key and a
		// per-run counter so it is unique AND short enough not to be truncated
		// into a collision (which is what used to happen with one job per step).
		Name:      o.executorName(run, inst),
		Namespace: run.Namespace,
		Owner:     run.Owner,
		Image:     imageFor(inst.Job),
		Commands:  []string{script},
		// The artifact uploader reads these at run time; the TOKEN comes from the
		// secret refs, never from here.
		Env: withArtifactEnv(baseEnv, o.artifactBase(), artifactPath(run, inst), len(targets) > 0),
		// Secrets are NEVER in Env: only refs, so the value cannot leak into the
		// spec. (The orchestrator keeps `secretEnv` solely to create the per-run
		// Secret the reference points at.)
		SecretEnv:      secretRefs,
		Shell:          "bash",
		TimeoutSeconds: jobTimeoutSeconds(inst.Job),
		Checkout:       nil, // the script performs the checkout itself
		Services:       serviceSpecs(inst.Job),
	}

	// The job runs under a context this orchestrator owns, so a CANCELLATION can
	// interrupt it: canceling the context stops WaitJob, and DeleteJob then kills
	// the container. Waiting only at step boundaries (the old behaviour) meant a
	// 30-minute job ignored a cancel for 30 minutes.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	if o.canceled(ctx, run.ID) {
		return instanceResult{state: RunCanceled}
	}
	// Watch for a cancellation request in the background and cancel the job.
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopWatch:
				return
			case <-runCtx.Done():
				return
			case <-ticker.C:
				if o.canceled(context.Background(), run.ID) {
					cancelRun()
					return
				}
			}
		}
	}()

	jobID, err := ops.RunJob(runCtx, spec)
	if err != nil {
		return instanceResult{state: RunFailed, err: err.Error()}
	}

	// Stream the log when the backend supports it, so a long step shows progress
	// live instead of surfacing only after it ends.
	var streamMu sync.Mutex
	var streamed strings.Builder
	streamDone := make(chan struct{})
	if ls, ok := ops.(LogStreamer); ok {
		go func() {
			defer close(streamDone)
			_ = ls.WatchLogs(runCtx, jobID, func(chunk string) {
				streamMu.Lock()
				streamed.WriteString(chunk)
				streamMu.Unlock()
				o.emitLog(run.ID, inst.Key, chunk)
			})
		}()
	} else {
		close(streamDone)
	}

	code, werr := ops.WaitJob(runCtx, jobID)
	// Killing the job is what makes a cancellation real; it is also the normal
	// cleanup path.
	_ = ops.DeleteJob(context.Background(), jobID)
	<-streamDone
	logs, _ := ops.Logs(context.Background(), jobID)
	streamMu.Lock()
	streamedLog := streamed.String()
	streamMu.Unlock()
	if logs == "" {
		logs = streamedLog
	}

	out := instanceResult{exit: code}
	if runCtx.Err() != nil || (werr != nil && strings.Contains(werr.Error(), "cancel")) {
		out.state = RunCanceled
		return out
	}

	states, rcs, outputs := parseSupervisorLog(logs, len(plan))
	out.steps = make([]StepResult, 0, len(plan))
	for i, sp := range plan {
		sr := StepResult{ID: sp.id, Name: sp.name}
		sr.State = states[i]
		sr.ExitCode = rcs[i]
		if outs, ok := outputs[i]; ok {
			sr.Outputs = outs
		}
		if sr.State == "" {
			// The result block was missing (killed before it could be written):
			// fall back to the exit code so the UI is not left blank.
			if code == 0 {
				sr.State = RunSuccess
			} else {
				sr.State = RunFailed
			}
		}
		out.steps = append(out.steps, sr)
		if sr.State == RunFailed && !sp.continueOnErr && out.state != RunFailed {
			out.state = RunFailed
			out.exit = sr.ExitCode
			if out.exit == 0 {
				out.exit = 1
			}
		}
	}
	if out.state == "" {
		if code == 0 {
			out.state = RunSuccess
		} else {
			out.state = RunFailed
		}
	}
	out.tail = joinTail("", logs, o.logCap)

	// Resolve the job's declared outputs from the collected step outputs.
	if len(j.Outputs) > 0 {
		stepsCtx := map[string]any{}
		for _, sr := range out.steps {
			if sr.ID != "" {
				stepsCtx[sr.ID] = map[string]any{"outputs": toAnyMapStrings(sr.Outputs), "outcome": string(sr.State)}
			}
		}
		vars["steps"] = stepsCtx
		octx := &expr.Ctx{Vars: vars, Status: expr.StatusSuccess}
		out.outputs = map[string]string{}
		for name, tmpl := range j.Outputs {
			if s, ierr := expr.Interpolate(tmpl, octx); ierr == nil {
				out.outputs[name] = s
			}
		}
	}
	return out
}

// stepBody renders one step's script (an action or a run block).
func (o *Orchestrator) stepBody(inst *Instance, st *Step, vars map[string]any) (string, error) {
	if st.Uses != "" {
		a, err := ResolveAction(st.Uses, inst.Job, st)
		if err != nil {
			return "", err
		}
		return a.Script, nil
	}
	return expr.Interpolate(st.Run, &expr.Ctx{Vars: vars})
}

// executorName builds a unique, short executor job name for one instance.
//
// The old scheme appended the STEP index to a string that sanitize() truncates to
// 60 characters, so every step of a job produced the SAME name and the second
// step failed with "job already exists". A per-orchestrator sequence makes the
// name unique regardless of what the workflow is called.
func (o *Orchestrator) executorName(run *Run, inst *Instance) string {
	o.mu.Lock()
	o.execSeq++
	seq := o.execSeq
	o.mu.Unlock()
	// A short, unique suffix that survives sanitize's 60-char cap: the sequence is
	// at the END so truncation removes the description, never the discriminator.
	return sanitize(fmt.Sprintf("ci-%s-%d", inst.Key, seq))
}

// parseSupervisorLog extracts the per-step states, exit codes and outputs from
// the supervisor's result blocks.
func parseSupervisorLog(logs string, n int) ([]RunState, []int, map[int]map[string]string) {
	states := make([]RunState, n)
	rcs := make([]int, n)
	outputs := map[int]map[string]string{}

	lines := strings.Split(logs, "\n")
	inOutputs := -1
	for _, raw := range lines {
		line := strings.TrimRight(raw, "\r")
		if inOutputs >= 0 {
			if strings.HasPrefix(line, "##[easydev:") {
				inOutputs = -1
				continue
			}
			k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
			if ok && k != "" {
				if outputs[inOutputs] == nil {
					outputs[inOutputs] = map[string]string{}
				}
				outputs[inOutputs][k] = v
			}
			continue
		}
		if !strings.HasPrefix(line, resultMarker) && !strings.HasPrefix(line, outputMarker) {
			continue
		}
		if strings.HasPrefix(line, outputMarker) {
			if idx, ok := secondField(line); ok {
				inOutputs = idx
			}
			continue
		}
		// `##[easydev:result] <idx> <state> <rc>` (or the "summary" line).
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[1] == "summary" {
			continue
		}
		idx, err := strconv.Atoi(fields[1])
		if err != nil || idx < 0 || idx >= n {
			continue
		}
		states[idx] = RunState(fields[2])
		if rc, err := strconv.Atoi(fields[3]); err == nil {
			rcs[idx] = rc
		}
	}
	return states, rcs, outputs
}

// secondField parses "##[easydev:step-outputs] <idx>".
func secondField(line string) (int, bool) {
	f := strings.Fields(line)
	if len(f) < 2 {
		return 0, false
	}
	i, err := strconv.Atoi(f[1])
	return i, err == nil
}

// jobTimeoutSeconds bounds a whole job instance.
func jobTimeoutSeconds(j *WorkflowJob) int {
	if j.TimeoutMinutes > 0 {
		return j.TimeoutMinutes * 60
	}
	return 0
}

// stepName is the display name of a step.
func stepName(st *Step, i int) string {
	if st.Name != "" {
		return st.Name
	}
	if st.Uses != "" {
		return st.Uses
	}
	if st.ID != "" {
		return st.ID
	}
	return fmt.Sprintf("step %d", i+1)
}
