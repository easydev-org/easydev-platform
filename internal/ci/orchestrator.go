package ci

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Event is what easyvcs reports when something happened that may start a
// pipeline. The gateway translates easyvcs callbacks into this.
type Event struct {
	Namespace string `json:"namespace"`
	Trigger   string `json:"trigger"` // rr_opened | rr_synced | rr_merged | tag | manual
	Ref       string `json:"ref"`
	Base      string `json:"base"`
	Head      string `json:"head"`
	RR        int64  `json:"rr"`
	Owner     string `json:"owner"`
	// YAML is the pipeline definition (the `.easydev-ci.yml` at Head). It is
	// accepted as a JSON STRING (`yaml`) so an HTTP caller can post it; the
	// gateway may also fetch it from easyvcs itself, so the orchestrator never
	// has to talk to VCS.
	YAML string `json:"yaml,omitempty"`
}

// Orchestrator runs pipelines. It is stateless apart from its Store: every
// decision is a function of (event, pipeline, store, ops).
type Orchestrator struct {
	store Store
	ops   OpsClient
	// MaxParallel caps concurrently running jobs within a stage.
	MaxParallel int
	// now is injectable for tests.
	now func() time.Time
	// opsFor, when set, returns a PER-RUN OpsClient bound to the run's tenant.
	// This is how multi-tenant works: every run's jobs land under its own
	// tenant, and a run can never touch another tenant's k8s objects.
	opsFor func(tenant string) OpsClient
}

// NewOrchestrator wires an orchestrator.
func NewOrchestrator(store Store, ops OpsClient) *Orchestrator {
	return &Orchestrator{store: store, ops: ops, MaxParallel: 4, now: time.Now}
}

// WithPerTenantOps binds the orchestrator to a FACTORY: every run uses a client
// scoped to its own tenant (namespace). nil restores the single-client mode.
func (o *Orchestrator) WithPerTenantOps(fn func(tenant string) OpsClient) *Orchestrator {
	o.opsFor = fn
	return o
}

// opsForRun resolves the OpsClient for a run: the per-tenant one when a factory
// is configured, else the single shared client.
func (o *Orchestrator) opsForRun(ns string) OpsClient {
	if o.opsFor != nil {
		if c := o.opsFor(ns); c != nil {
			return c
		}
	}
	return o.ops
}

// Run starts a pipeline for an event. It returns the created run id and runs
// the stages asynchronously (the caller polls the store). Each finished stage
// updates the RR's check so a merge can be blocked.
func (o *Orchestrator) Run(ctx context.Context, ev Event) (string, error) {
	trigger, ok := ParseTrigger(ev.Trigger)
	if !ok {
		return "", fmt.Errorf("ci: unknown trigger %q", ev.Trigger)
	}
	pipe, err := ParseYAML([]byte(ev.YAML))
	if err != nil {
		return "", err
	}
	if !pipe.TriggeredBy(trigger) {
		return "", fmt.Errorf("ci: pipeline does not trigger on %q", trigger)
	}
	stages, err := pipe.Order()
	if err != nil {
		return "", err
	}
	now := o.now().UTC().UnixMilli()
	run := &Run{
		ID:        fmt.Sprintf("run-%d-%s", now, sanitize(ev.Namespace+"-"+ev.Ref)),
		Namespace: ev.Namespace, Pipeline: pipe.Name, Trigger: trigger,
		Ref: ev.Ref, Base: ev.Base, Head: ev.Head, RR: ev.RR,
		State: RunRunning, Created: now, Owner: ev.Owner,
	}
	for _, stage := range stages {
		for _, id := range stage {
			run.Jobs = append(run.Jobs, JobResult{JobID: id, State: RunPending})
		}
	}
	if err := o.store.PutRun(ctx, run); err != nil {
		return "", err
	}
	// Required if ANY job is required: the check blocks the merge unless every
	// required job succeeded.
	required := false
	for _, j := range pipe.Jobs {
		if j.Required {
			required = true
		}
	}
	_ = o.store.PutCheck(ctx, &Check{
		RR: ev.RR, Namespace: ev.Namespace, Name: pipe.Name,
		State: RunRunning, Required: required, RunID: run.ID, Details: "running",
	})
	go o.execute(context.Background(), run, pipe, stages, o.opsForRun(ev.Namespace))
	return run.ID, nil
}

func (o *Orchestrator) execute(ctx context.Context, run *Run, pipe *Pipeline, stages [][]string, ops OpsClient) {
	byID := map[string]*JobResult{}
	for i := range run.Jobs {
		byID[run.Jobs[i].JobID] = &run.Jobs[i]
	}
	failed := false
	for _, stage := range stages {
		if failed {
			// A previous stage failed: mark the rest as skipped.
			for _, id := range stage {
				if r, ok := byID[id]; ok {
					r.State = RunCanceled
				}
			}
			continue
		}
		sem := make(chan struct{}, maxInt(o.MaxParallel, 1))
		var wg sync.WaitGroup
		var mu sync.Mutex
		for _, id := range stage {
			wg.Add(1)
			go func(id string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				code, logs, err := o.runOne(ctx, run, pipe.Jobs[id], ops)
				mu.Lock()
				defer mu.Unlock()
				r := byID[id]
				r.Finished = o.now().UTC().UnixMilli()
				r.ExitCode = code
				r.LogsTail = logs
				if err != nil {
					r.State = RunFailed
					r.Error = err.Error()
					failed = true
				} else if code != 0 {
					r.State = RunFailed
					failed = true
				} else {
					r.State = RunSuccess
				}
			}(id)
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
	_ = o.store.PutCheck(ctx, &Check{
		RR: run.RR, Namespace: run.Namespace, Name: run.Pipeline,
		State: run.State, Required: o.anyRequired(pipe), RunID: run.ID, Details: details,
	})
}

func (o *Orchestrator) runOne(ctx context.Context, run *Run, j *Job, ops OpsClient) (int, string, error) {
	spec := JobSpec{
		Name:      sanitize(fmt.Sprintf("%s-%s-%s", run.ID, j.ID, run.Head[:minInt(8, len(run.Head))])),
		Namespace: run.Namespace, Owner: run.Owner, Image: j.Image,
		Commands: j.Commands, Env: j.Env, CPU: j.CPU, Memory: j.Memory,
		TimeoutSeconds: j.TimeoutSeconds,
		Checkout:       checkoutURL(run),
	}
	id, err := ops.RunJob(ctx, spec)
	if err != nil {
		return 0, "", err
	}
	code, err := ops.WaitJob(ctx, id)
	if err != nil {
		return 0, "", err
	}
	logs, _ := ops.Logs(ctx, id)
	if len(logs) > 4096 {
		logs = logs[len(logs)-4096:]
	}
	_ = ops.DeleteJob(ctx, id)
	return code, logs, nil
}

// anyRequired reports whether any job in the pipeline is required.
func (o *Orchestrator) anyRequired(p *Pipeline) bool {
	for _, j := range p.Jobs {
		if j.Required {
			return true
		}
	}
	return false
}

// checkoutURL is the easyvcs source archive the job fetches before running.
// The gateway supplies the base URL via the Run's Head; here it is relative
// (the job's env carries EASYDEV_CHECKOUT_URL).
func checkoutURL(run *Run) string {
	if run.Head == "" {
		return ""
	}
	return "/api/v1/namespaces/" + run.Namespace + "/archive/" + run.Head + ".tar.gz"
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "run"
	}
	if len(out) > 60 {
		out = out[:60]
	}
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
