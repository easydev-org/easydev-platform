package ci

import (
	"context"
	"strings"
	"testing"
	"time"
)

const sampleYAML = `
version: 1
on: [rr_opened, rr_synced]
jobs:
  test:
    image: golang:1.27
    commands: ["go vet ./...", "go test ./..."]
    required: true
  build:
    needs: [test]
    image: golang:1.27
    commands: ["go build ./..."]
    artifacts: ["bin/*"]
`

func TestParseAndOrder(t *testing.T) {
	p, err := ParseYAML([]byte(sampleYAML))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Jobs) != 2 {
		t.Fatalf("jobs = %d", len(p.Jobs))
	}
	stages, err := p.Order()
	if err != nil {
		t.Fatal(err)
	}
	// test first (parallel-able), then build.
	if len(stages) != 2 || stages[0][0] != "test" || stages[1][0] != "build" {
		t.Fatalf("stages = %v", stages)
	}
	if !p.TriggeredBy(TriggerRROpened) || p.TriggeredBy(TriggerTag) {
		t.Fatalf("trigger matching wrong")
	}
	if !p.Jobs["test"].Required || p.Jobs["build"].Required {
		t.Fatalf("required flags wrong")
	}
}

func TestParseRejectsCycleAndDanglingNeeds(t *testing.T) {
	_, err := ParseYAML([]byte(`
on: [rr_opened]
jobs:
  a: {image: x, commands: [x], needs: [b]}
  b: {image: x, commands: [x], needs: [a]}
`))
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected a cycle error, got %v", err)
	}
	_, err = ParseYAML([]byte(`
on: [rr_opened]
jobs:
  a: {image: x, commands: [x], needs: [ghost]}
`))
	if err == nil || !strings.Contains(err.Error(), "unknown job") {
		t.Fatalf("expected an unknown-job error, got %v", err)
	}
	_, err = ParseYAML([]byte("jobs: {}"))
	if err == nil {
		t.Fatalf("expected an error for an empty pipeline")
	}
}

// TestOrchestratorSuccessAndChecks runs a pipeline through the fake ops backend
// and asserts the run + the RR check end up green.
func TestOrchestratorSuccessAndChecks(t *testing.T) {
	store := NewMemStore()
	ops := NewFakeOps()
	o := NewOrchestrator(store, ops)

	id, err := o.Run(context.Background(), Event{
		Namespace: "team-app", Trigger: "rr_opened", Ref: "feature",
		Base: "base-sha", Head: "head-sha", RR: 7, Owner: "alice",
		YAML: sampleYAML,
	})
	if err != nil {
		t.Fatal(err)
	}
	run := waitTerminal(t, store, id)
	if run.State != RunSuccess {
		t.Fatalf("run state = %s (jobs=%+v)", run.State, run.Jobs)
	}
	for _, j := range run.Jobs {
		if j.State != RunSuccess || j.ExitCode != 0 {
			t.Fatalf("job %s = %s/%d", j.JobID, j.State, j.ExitCode)
		}
	}
	checks, _ := store.ListChecks(context.Background(), "team-app", 7)
	if len(checks) != 1 || checks[0].State != RunSuccess || !checks[0].Required {
		t.Fatalf("checks = %+v", checks)
	}
}

// TestOrchestratorStopsOnRequiredFailure asserts a failed job fails the run and
// marks the downstream job canceled, and the check goes red (blocks the merge).
func TestOrchestratorStopsOnRequiredFailure(t *testing.T) {
	store := NewMemStore()
	ops := NewFakeOps()
	// "test" job is named "<run>-test-..."; fail any job whose name contains "-test-".
	ops.FailJob = func(name string) int {
		if strings.Contains(name, "-test-") {
			return 1
		}
		return 0
	}
	o := NewOrchestrator(store, ops)
	id, err := o.Run(context.Background(), Event{
		Namespace: "team-app", Trigger: "rr_opened", Ref: "feature",
		Head: "deadbeefdeadbeef", RR: 9, Owner: "alice", YAML: sampleYAML,
	})
	if err != nil {
		t.Fatal(err)
	}
	run := waitTerminal(t, store, id)
	if run.State != RunFailed {
		t.Fatalf("run state = %s", run.State)
	}
	var build JobResult
	for _, j := range run.Jobs {
		if j.JobID == "build" {
			build = j
		}
	}
	if build.State != RunCanceled {
		t.Fatalf("downstream build should be canceled, got %s", build.State)
	}
	checks, _ := store.ListChecks(context.Background(), "team-app", 9)
	if len(checks) != 1 || checks[0].State != RunFailed || !checks[0].Required {
		t.Fatalf("a required failing check must be red: %+v", checks)
	}
}

func TestRunSkipsWhenTriggerNotMatched(t *testing.T) {
	o := NewOrchestrator(NewMemStore(), NewFakeOps())
	_, err := o.Run(context.Background(), Event{
		Namespace: "n", Trigger: "tag", Head: "x", YAML: sampleYAML,
	})
	if err == nil || !strings.Contains(err.Error(), "does not trigger") {
		t.Fatalf("expected a does-not-trigger error, got %v", err)
	}
}

// waitTerminal polls the store until the run finishes (or times out).
func waitTerminal(t *testing.T, s Store, id string) *Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, err := s.GetRun(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if r.State == RunSuccess || r.State == RunFailed || r.State == RunCanceled {
			return r
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run %s did not finish", id)
	return nil
}
