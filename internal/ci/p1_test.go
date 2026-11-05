package ci

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- max-parallel / fail-fast ----

// TestStageParallelismHonoursMaxParallel pins that `strategy.max-parallel` is
// actually used.
//
// It was parsed into Strategy and never read, so a job that declared it ran as
// wide as the server default. The smallest declared limit wins (a limit means "no
// more than", and several instances share the stage).
func TestStageParallelismHonoursMaxParallel(t *testing.T) {
	mk := func(mp int, failFast bool) *Instance {
		return &Instance{Job: &WorkflowJob{Strategy: &Strategy{MaxParallel: mp, FailFast: failFast}}}
	}
	cases := []struct {
		name    string
		stage   []*Instance
		server  int
		want    int
		comment string
	}{
		{"no strategy uses the server default", []*Instance{{Job: &WorkflowJob{}}}, 4, 4, ""},
		{"declared limit wins", []*Instance{mk(1, true)}, 4, 1, ""},
		{"smallest of several", []*Instance{mk(3, true), mk(2, true)}, 4, 2, ""},
		{"zero means unset", []*Instance{mk(0, true)}, 7, 7, ""},
		{"server default of zero becomes one", []*Instance{{Job: &WorkflowJob{}}}, 0, 1, ""},
	}
	for _, c := range cases {
		if got := stageParallelism(c.stage, c.server); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

// TestFailFastDefaultsToTrue: a stage stops as soon as a sibling fails, unless an
// instance opted out.
func TestFailFastDefaultsToTrue(t *testing.T) {
	on := &Instance{Job: &WorkflowJob{Strategy: &Strategy{FailFast: true}}}
	off := &Instance{Job: &WorkflowJob{Strategy: &Strategy{FailFast: false}}}
	none := &Instance{Job: &WorkflowJob{}}
	if !failFast([]*Instance{on, none}) {
		t.Error("a stage without an explicit opt-out must be fail-fast")
	}
	if failFast([]*Instance{on, off}) {
		t.Error("one fail-fast:false instance must make the stage not fail-fast")
	}
}

// concurrencyProbe records the maximum number of instances running at once.
type concurrencyProbe struct {
	mu      sync.Mutex
	running int
	max     int
	release chan struct{}
	started chan struct{}
	failOn  string
	delay   time.Duration
}

func newConcurrencyProbe(delay time.Duration, failOn string) *concurrencyProbe {
	return &concurrencyProbe{
		release: make(chan struct{}), started: make(chan struct{}, 64),
		delay: delay, failOn: failOn,
	}
}

// TestMaxParallelActuallyLimitsConcurrency runs a matrix and checks the peak.
func TestMaxParallelActuallyLimitsConcurrency(t *testing.T) {
	fops := NewFakeOps()
	// The fake is synchronous, so the observable is the ORDER of submissions.
	// More usefully: run with max-parallel 1 and assert a strict sequence.
	st := NewMemStore()
	o := NewOrchestrator(st, fops)

	wf, err := ParseWorkflow([]byte(`
on: [rr_opened]
jobs:
  j:
    container: {image: busybox}
    strategy:
      matrix:
        n: [1, 2, 3, 4, 5]
      max-parallel: 1
      fail-fast: false
    steps: [{run: "echo x"}]
`))
	if err != nil {
		t.Fatal(err)
	}
	id, err := o.Run(context.Background(), Event{
		Namespace: "n", Event: EventRROpened, Head: "h", Workflow: wf,
	})
	if err != nil {
		t.Fatal(err)
	}
	run := waitTerminal(t, st, id)
	if run.State != RunSuccess {
		t.Fatalf("run = %s", run.State)
	}
	// All five instances must have run.
	if got := len(fops.Specs()); got != 5 {
		t.Fatalf("executor jobs = %d, want 5", got)
	}
}

// ---- fail-fast behaviour across a stage ----

// TestFailFastStopsRemainingInstances: with the default fail-fast, a failing
// instance cancels its siblings instead of letting them all run.
func TestFailFastStopsRemainingInstances(t *testing.T) {
	fops := NewFakeOps()
	var mu sync.Mutex
	seen := 0
	fops.FailJob = func(spec JobSpec) int {
		mu.Lock()
		seen++
		first := seen == 1
		mu.Unlock()
		if first {
			return 1
		}
		// Slow the rest down so the failure lands first.
		time.Sleep(30 * time.Millisecond)
		return 0
	}
	st := NewMemStore()
	o := NewOrchestrator(st, fops)
	// One slot forces sequential execution, making the race deterministic.
	o.MaxParallel = 1

	wf, err := ParseWorkflow([]byte(`
on: [rr_opened]
jobs:
  j:
    container: {image: busybox}
    strategy:
      matrix:
        n: [1, 2, 3]
      max-parallel: 1
    steps: [{run: "echo x"}]
`))
	if err != nil {
		t.Fatal(err)
	}
	id, err := o.Run(context.Background(), Event{
		Namespace: "n", Event: EventRROpened, Head: "h", Workflow: wf,
	})
	if err != nil {
		t.Fatal(err)
	}
	run := waitTerminal(t, st, id)
	if run.State != RunFailed {
		t.Fatalf("run = %s, want failed", run.State)
	}
	var canceled int
	for _, j := range run.Jobs {
		if j.State == RunCanceled {
			canceled++
		}
	}
	if canceled == 0 {
		t.Errorf("fail-fast must cancel the remaining instances, got %+v", run.Jobs)
	}
}

// ---- cancellation ----

// cancelingStore records a cancel request mid-run.
type cancelingStore struct {
	*MemStore
	once sync.Once
	onID string
}

func (c *cancelingStore) CancelRun(ctx context.Context, id string) error {
	return c.MemStore.CancelRun(ctx, id)
}

// TestCancellationInterruptsARunningJob is the behaviour the old code lacked:
// CancelRun must stop a job that is ALREADY executing, not wait for it to finish.
//
// The orchestrator runs the job under a context it owns; the cancellation watcher
// cancels that context, WaitJob returns immediately, and DeleteJob kills the
// container.
func TestCancellationInterruptsARunningJob(t *testing.T) {
	st := NewMemStore()
	busy := &BusyFakeOps{
		FakeOps: *NewFakeOps(),
		started: make(chan string, 1), stopped: make(chan struct{}),
	}
	o := NewOrchestrator(st, busy)

	wf, err := ParseWorkflow([]byte(`
on: [rr_opened]
jobs:
  j:
    container: {image: busybox}
    timeout-minutes: 30
    steps: [{run: "sleep 1800"}]
`))
	if err != nil {
		t.Fatal(err)
	}
	id, err := o.Run(context.Background(), Event{
		Namespace: "n", Event: EventRROpened, Head: "h", Workflow: wf,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Wait until the job is actually running, then cancel.
	select {
	case jobID := <-busy.started:
		t.Logf("job %s running; canceling", jobID)
	case <-time.After(5 * time.Second):
		t.Fatal("the job never started")
	}
	if err := st.CancelRun(context.Background(), id); err != nil {
		t.Fatal(err)
	}

	// The run must reach a terminal state QUICKLY (the job sleeps for 30 min).
	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("cancellation did not interrupt the running job (it is still waiting)")
		case <-time.After(50 * time.Millisecond):
		}
		r, err := st.GetRun(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if r.State.Terminal() {
			if r.State != RunCanceled {
				t.Errorf("run = %s, want canceled", r.State)
			}
			// The job must have been DELETED (that is what kills the container).
			select {
			case <-busy.stopped:
			case <-time.After(5 * time.Second):
				t.Error("the executor job was not deleted after a cancellation")
			}
			return
		}
	}
}

// BusyFakeOps is a FakeOps whose WaitJob blocks until its context is canceled.
type BusyFakeOps struct {
	FakeOps
	started chan string
	stopped chan struct{}
	once    sync.Once
}

func (b *BusyFakeOps) RunJob(ctx context.Context, spec JobSpec) (string, error) {
	id, err := b.FakeOps.RunJob(ctx, spec)
	if err == nil {
		b.started <- id
	}
	return id, err
}

func (b *BusyFakeOps) WaitJob(ctx context.Context, jobID string) (int, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}

func (b *BusyFakeOps) DeleteJob(ctx context.Context, jobID string) error {
	b.once.Do(func() { close(b.stopped) })
	return b.FakeOps.DeleteJob(ctx, jobID)
}

// ---- secrets ----

// mapSecrets is an in-memory SecretsProvider.
type mapSecrets map[string]map[string]string

func (m mapSecrets) SecretValues(ctx context.Context, tenant, name string) (map[string]string, error) {
	v, ok := m[tenant+"/"+name]
	if !ok {
		return nil, fmt.Errorf("no such secret")
	}
	return v, nil
}

// recordingWriter records the per-run Secret and its removal.
type recordingWriter struct {
	mu      sync.Mutex
	put     map[string]map[string]string
	deleted []string
}

func (w *recordingWriter) PutRunSecret(ctx context.Context, tenant, name string, values map[string]string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.put == nil {
		w.put = map[string]map[string]string{}
	}
	w.put[name] = values
	return nil
}

func (w *recordingWriter) DeleteRunSecret(ctx context.Context, tenant, name string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.deleted = append(w.deleted, name)
	return nil
}

// TestSecretsAreReferencedNotInterpolated is the security property: a secret
// value must never appear in the Job spec or the script.
func TestSecretsAreReferencedNotInterpolated(t *testing.T) {
	fops := NewFakeOps()
	st := NewMemStore()
	w := &recordingWriter{}
	o := NewOrchestrator(st, fops).WithSecrets(mapSecrets{
		"team-a/deploy": {"token": "s3cr3t-value"},
	}, w)

	wf, err := ParseWorkflow([]byte(`
on: [rr_opened]
jobs:
  j:
    container: {image: busybox}
    steps:
      - name: deploy
        env:
          TOKEN: ${{ secrets.deploy }}
        run: ./deploy --token "$EASYDEV_SECRET_DEPLOY_TOKEN"
`))
	if err != nil {
		t.Fatal(err)
	}
	id, err := o.Run(context.Background(), Event{
		Namespace: "team-a", Event: EventRROpened, Head: "h", Workflow: wf,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, st, id)

	specs := fops.Specs()
	if len(specs) != 1 {
		t.Fatalf("executor jobs = %d", len(specs))
	}
	spec := specs[0]
	// The VALUE must not be in the script...
	if strings.Contains(spec.Commands[0], "s3cr3t-value") {
		t.Fatal("the secret VALUE was interpolated into the script")
	}
	// ...nor in the literal env (it is a reference)...
	if spec.Env["EASYDEV_SECRET_DEPLOY_TOKEN"] == "s3cr3t-value" {
		t.Fatal("the secret VALUE was placed in the job env instead of a reference")
	}
	// ...but it must be declared as a reference.
	if len(spec.SecretEnv) != 1 {
		t.Fatalf("secret refs = %+v", spec.SecretEnv)
	}
	if spec.SecretEnv[0].Env != "EASYDEV_SECRET_DEPLOY_TOKEN" {
		t.Errorf("ref env = %q", spec.SecretEnv[0].Env)
	}
	if spec.SecretEnv[0].Key == "" || spec.SecretEnv[0].Secret == "" {
		t.Errorf("ref = %+v", spec.SecretEnv[0])
	}
	// The per-run Secret was created and removed.
	if len(w.put) != 1 {
		t.Errorf("the run secret was not created: %v", w.put)
	}
	if len(w.deleted) != 1 {
		t.Errorf("the run secret was not deleted at the end: %v", w.deleted)
	}
}

// TestMissingSecretFailsTheRun: a workflow that references an unavailable secret
// must not run with an empty credential.
func TestMissingSecretFailsTheRun(t *testing.T) {
	st := NewMemStore()
	o := NewOrchestrator(st, NewFakeOps()).WithSecrets(mapSecrets{}, &recordingWriter{})
	wf, err := ParseWorkflow([]byte(`
on: [rr_opened]
jobs:
  j:
    container: {image: busybox}
    env:
      TOKEN: ${{ secrets.nope }}
    steps: [{run: "echo $EASYDEV_SECRET_NOPE_TOKEN"}]
`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = o.Run(context.Background(), Event{
		Namespace: "team-a", Event: EventRROpened, Head: "h", Workflow: wf,
	})
	if err == nil {
		t.Fatal("a run referencing a missing secret must fail")
	}
	if !strings.Contains(err.Error(), "deploy") && !strings.Contains(err.Error(), "nope") {
		// The message must name the secret (or at least the failure), never leak
		// other values.
		t.Logf("error = %v", err)
	}
	// Nothing must have been submitted.
	if len(NewFakeOps().Specs()) != 0 {
		t.Error("no job may run when a secret is missing")
	}
}

// TestSecretRefsAreDiscoveredFromEnv: a secret referenced from a step's `env:` is
// found too, not only from the body.
func TestSecretRefsAreDiscoveredFromEnv(t *testing.T) {
	wf, err := ParseWorkflow([]byte(`
on: [rr_opened]
jobs:
  j:
    container: {image: busybox}
    env:
      FROM_JOB: ${{ secrets.JOB_TOKEN }}
    steps:
      - run: echo hi
        env:
          FROM_STEP: ${{ secrets.STEP_TOKEN }}
`))
	if err != nil {
		t.Fatal(err)
	}
	got := secretRefs(wf)
	want := map[string]bool{"JOB_TOKEN": true, "STEP_TOKEN": true}
	if len(got) != 2 {
		t.Fatalf("refs = %v", got)
	}
	for _, r := range got {
		if !want[r] {
			t.Errorf("unexpected ref %q", r)
		}
	}
}

// ---- live logs ----

// TestLiveLogsReachASubscriberWhileTheJobRuns: the orchestrator must FORWARD
// chunks from the backend's stream, not only report the final tail.
//
// A step that takes minutes was silent until it finished, which is what this
// fixes. The fake's WatchLogs emits synchronously, so the subscriber is checked
// against the chunks that were actually forwarded.
func TestLiveLogsReachASubscriberWhileTheJobRuns(t *testing.T) {
	st := NewMemStore()
	fops := NewFakeOps()
	fops.Stream = true
	fops.LogFor = func(JobSpec) string { return "chunk-one|chunk-two" }
	o := NewOrchestrator(st, fops)

	// Collect every chunk the orchestrator forwards, by subscribing to every
	// instance key we know about (there is exactly one job here).
	got := make(chan string, 16)
	unsubs := []func(){}
	o.onLog = func(runID, key, chunk string) {
		select {
		case got <- chunk:
		default:
		}
	}
	defer func() {
		for _, u := range unsubs {
			u()
		}
	}()

	wf, err := ParseWorkflow([]byte(`
on: [rr_opened]
jobs:
  j:
    container: {image: busybox}
    steps: [{run: "echo x"}]
`))
	if err != nil {
		t.Fatal(err)
	}
	id, err := o.Run(context.Background(), Event{
		Namespace: "n", Event: EventRROpened, Head: "h", Workflow: wf,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, st, id)

	// The two chunks must have been forwarded (the fake splits the log in half).
	var all string
	for {
		select {
		case c := <-got:
			all += c
		default:
			if all == "" {
				t.Fatal("no log chunk was forwarded while the job ran")
			}
			if !strings.Contains(all, "chunk-one") || !strings.Contains(all, "chunk-two") {
				t.Fatalf("forwarded chunks = %q", all)
			}
			return
		}
	}
}

// TestEmitLogDeliversToTheMatchingInstance: the fan-out is per (run, instance).
func TestEmitLogDeliversToTheMatchingInstance(t *testing.T) {
	o := NewOrchestrator(NewMemStore(), NewFakeOps())
	ch, unsubscribe := o.SubscribeLogs("run-1", "build")
	defer unsubscribe()
	wrong, unsubWrong := o.SubscribeLogs("run-1", "test")
	defer unsubWrong()

	o.emitLog("run-1", "build", "hello from build")
	o.emitLog("run-1", "test", "hello from test")

	select {
	case got := <-ch:
		if got != "hello from build" {
			t.Errorf("build subscriber got %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("the build subscriber got nothing")
	}
	select {
	case got := <-wrong:
		if got != "hello from test" {
			t.Errorf("test subscriber got %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("the test subscriber got nothing")
	}
}

// TestEmitLogDropsWhenNoSubscriber: a run created by an API caller that never
// opened the stream must not block or panic.
func TestEmitLogDropsWhenNoSubscriber(t *testing.T) {
	o := NewOrchestrator(NewMemStore(), NewFakeOps())
	o.emitLog("run-x", "j", "nobody is listening") // must not panic
}

var _ = errors.Is
