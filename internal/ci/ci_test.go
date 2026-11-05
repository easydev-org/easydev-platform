package ci

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

const sampleWorkflow = `
version: 1
name: ci
on:
  rr:
    types: [opened, synced]
jobs:
  test:
    runs-on: [linux]
    container: {image: golang:1.27}
    required: true
    steps:
      - name: vet
        run: go vet ./...
      - name: test
        run: go test ./...
  build:
    needs: [test]
    container: {image: golang:1.27}
    steps:
      - run: go build ./...
`

// v1Workflow is the legacy shape: `commands:` instead of `steps:`.
const v1Workflow = `
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
`

func TestParseWorkflow(t *testing.T) {
	w, err := ParseWorkflow([]byte(sampleWorkflow))
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Jobs) != 2 {
		t.Fatalf("jobs = %d", len(w.Jobs))
	}
	if !w.Jobs["test"].Required || w.Jobs["build"].Required {
		t.Fatal("required flags wrong")
	}
	if got := len(w.Jobs["test"].Steps); got != 2 {
		t.Fatalf("test steps = %d, want 2", got)
	}
	plan, err := w.Plan()
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Stages) != 2 || plan.Stages[0][0].Key != "test" || plan.Stages[1][0].Key != "build" {
		t.Fatalf("stages = %v", plan.Stages)
	}
	if !plan.Required {
		t.Fatal("plan should be required")
	}
}

func TestParseV1CommandsShape(t *testing.T) {
	w, err := ParseWorkflow([]byte(v1Workflow))
	if err != nil {
		t.Fatal(err)
	}
	j := w.Jobs["test"]
	if j.Container == nil || j.Container.Image != "golang:1.27" {
		t.Fatalf("container = %+v", j.Container)
	}
	if len(j.Steps) != 1 || !strings.Contains(j.Steps[0].Run, "go vet") {
		t.Fatalf("steps = %+v", j.Steps)
	}
	// A bare `on: [..]` list must map to the right triggers.
	if !w.TriggeredBy(&Event{Event: EventRROpened}) {
		t.Fatal("on: [rr_opened] should react to an RR open")
	}
	if w.TriggeredBy(&Event{Event: EventTag}) {
		t.Fatal("on: [rr_opened] must not react to a tag")
	}
}

func TestParseRejectsCycleAndDanglingNeeds(t *testing.T) {
	_, err := ParseWorkflow([]byte(`
on: [rr_opened]
jobs:
  a: {image: x, commands: [x], needs: [b]}
  b: {image: x, commands: [x], needs: [a]}
`))
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected a cycle error, got %v", err)
	}
	_, err = ParseWorkflow([]byte(`
on: [rr_opened]
jobs:
  a: {image: x, commands: [x], needs: [ghost]}
`))
	if err == nil || !strings.Contains(err.Error(), "unknown job") {
		t.Fatalf("expected an unknown-job error, got %v", err)
	}
	if _, err := ParseWorkflow([]byte("jobs: {}")); err == nil {
		t.Fatal("expected an error for an empty workflow")
	}
}

func TestMatrixExpansion(t *testing.T) {
	w, err := ParseWorkflow([]byte(`
on: [rr_opened]
jobs:
  test:
    runs-on: [linux]
    strategy:
      matrix:
        go: ["1.26", "1.27"]
        os: [linux, darwin]
        exclude:
          - {go: "1.26", os: darwin}
        include:
          - {go: "1.27", os: linux, experimental: true}
    steps:
      - run: go test ./...
`))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := w.Plan()
	if err != nil {
		t.Fatal(err)
	}
	// 4 combinations - 1 excluded = 3; include augments the existing 1.27/linux.
	if len(plan.Stages) != 1 || len(plan.Stages[0]) != 3 {
		t.Fatalf("instances = %d (%v)", len(plan.Stages[0]), plan.Stages[0])
	}
	keys := map[string]bool{}
	for _, i := range plan.Stages[0] {
		keys[i.Key] = true
	}
	for _, want := range []string{"test-go-1.26-os-linux", "test-go-1.27-os-linux", "test-go-1.27-os-darwin"} {
		if !keys[want] {
			t.Errorf("missing instance %q in %v", want, keys)
		}
	}
	// The include entry must AUGMENT, not add a fourth instance.
	if keys["test-go-1.27-os-linux-experimental-true"] {
		t.Error("include should merge into the existing combination")
	}
}

func TestGlobFilters(t *testing.T) {
	w, err := ParseWorkflow([]byte(`
on:
  rr:
    paths: ["src/**", "go.mod"]
    branches: [main]
jobs:
  t: {image: x, commands: [y]}
`))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		paths []string
		ref   string
		want  bool
	}{
		{[]string{"src/a/b.go"}, "main", true},
		{[]string{"go.mod"}, "main", true},
		{[]string{"docs/x.md"}, "main", false},
		{[]string{"src/a.go"}, "feature", false},
	}
	for _, c := range cases {
		got := w.TriggeredBy(&Event{Event: EventRROpened, ChangedPaths: c.paths, Ref: c.ref})
		if got != c.want {
			t.Errorf("paths=%v ref=%s -> %v, want %v", c.paths, c.ref, got, c.want)
		}
	}
}

func TestTagTrigger(t *testing.T) {
	w, err := ParseWorkflow([]byte(`
on:
  tag: {pattern: "v*"}
jobs:
  t: {image: x, commands: [y]}
`))
	if err != nil {
		t.Fatal(err)
	}
	if !w.TriggeredBy(&Event{Event: EventTag, Tag: "v1.2.3"}) {
		t.Error("v1.2.3 should match pattern v*")
	}
	if w.TriggeredBy(&Event{Event: EventTag, Tag: "nightly"}) {
		t.Error("nightly should not match v*")
	}
}

// TestOrchestratorSuccessAndChecks runs a workflow through the fake backend and
// asserts the run + the RR check end up green.
func TestOrchestratorSuccessAndChecks(t *testing.T) {
	st := NewMemStore()
	ops := NewFakeOps()
	o := NewOrchestrator(st, ops)

	wf, err := ParseWorkflow([]byte(sampleWorkflow))
	if err != nil {
		t.Fatal(err)
	}
	id, err := o.Run(context.Background(), Event{
		Namespace: "team-app", Event: EventRROpened, Ref: "feature",
		Base: "base-sha", Head: "head-sha", RR: 7, Owner: "alice", Workflow: wf,
	})
	if err != nil {
		t.Fatal(err)
	}
	run := waitTerminal(t, st, id)
	if run.State != RunSuccess {
		t.Fatalf("run state = %s (jobs=%+v)", run.State, run.Jobs)
	}
	for _, j := range run.Jobs {
		if j.State != RunSuccess || j.ExitCode != 0 {
			t.Fatalf("job %s = %s/%d", j.Key, j.State, j.ExitCode)
		}
	}
	// ONE executor job per job instance (all steps share it): test + build = 2.
	//
	// This is the whole point of the supervisor: a per-step executor job meant a
	// fresh empty workspace per step, so "compile then test" could not work.
	if got := len(ops.Specs()); got != 2 {
		t.Fatalf("executor calls = %d, want 2 (one per instance)", got)
	}
	// The combined script must carry BOTH of test's steps. Step bodies are
	// BASE64-ENCODED inside the script (a workflow may contain any text, so a
	// literal heredoc would be an injection vector), so the assertion decodes.
	script := ops.Specs()[0].Commands[0]
	decoded := decodeStepBodies(t, script)
	for _, want := range []string{"go vet ./...", "go test ./..."} {
		if !strings.Contains(decoded, want) {
			t.Errorf("the supervisor script is missing %q (decoded bodies:\n%s)", want, decoded)
		}
	}
	// The checkout is pinned to the head snapshot and happens ONCE, inside the
	// supervisor script (the executor no longer receives a Checkout).
	for _, s := range ops.Specs() {
		if !strings.Contains(s.Commands[0], "archive/head-sha.tar.gz") {
			t.Errorf("the script does not check out head-sha:\n%s", s.Commands[0])
		}
	}
	checks, _ := st.ListChecks(context.Background(), "team-app", 7)
	if len(checks) != 1 || checks[0].State != RunSuccess || !checks[0].Required {
		t.Fatalf("checks = %+v", checks)
	}
}

// TestOrchestratorStopsOnRequiredFailure asserts a failed job fails the run and
// marks the downstream job canceled, and the check goes red (blocks the merge).
func TestOrchestratorStopsOnRequiredFailure(t *testing.T) {
	st := NewMemStore()
	ops := NewFakeOps()
	ops.FailJob = func(spec JobSpec) int {
		if strings.Contains(spec.Name, "-test-") {
			return 1
		}
		return 0
	}
	o := NewOrchestrator(st, ops)
	wf, err := ParseWorkflow([]byte(sampleWorkflow))
	if err != nil {
		t.Fatal(err)
	}
	id, err := o.Run(context.Background(), Event{
		Namespace: "team-app", Event: EventRROpened, Ref: "feature",
		Head: "deadbeefdeadbeef", RR: 9, Owner: "alice", Workflow: wf,
	})
	if err != nil {
		t.Fatal(err)
	}
	run := waitTerminal(t, st, id)
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
	checks, _ := st.ListChecks(context.Background(), "team-app", 9)
	if len(checks) != 1 || checks[0].State != RunFailed || !checks[0].Required {
		t.Fatalf("a required failing check must be red: %+v", checks)
	}
}

func TestRunSkipsWhenTriggerNotMatched(t *testing.T) {
	o := NewOrchestrator(NewMemStore(), NewFakeOps())
	wf, err := ParseWorkflow([]byte(sampleWorkflow))
	if err != nil {
		t.Fatal(err)
	}
	_, err = o.Run(context.Background(), Event{
		Namespace: "n", Event: EventTag, Tag: "v1", Head: "x", Workflow: wf,
	})
	if err == nil || !strings.Contains(err.Error(), "does not trigger") {
		t.Fatalf("expected a does-not-trigger error, got %v", err)
	}
}

// TestRunRequiresResolvedWorkflow asserts the orchestrator never accepts YAML
// from an event body: the gateway must resolve it from the VCS first.
func TestRunRequiresResolvedWorkflow(t *testing.T) {
	o := NewOrchestrator(NewMemStore(), NewFakeOps())
	if _, err := o.Run(context.Background(), Event{Namespace: "n", Event: EventRROpened}); err == nil {
		t.Fatal("a run without a resolved workflow must fail")
	}
}

// TestJobOutputsAndIf asserts step outputs feed `needs.*.outputs` and that an
// `if:` can gate a downstream job.
func TestJobOutputsAndIf(t *testing.T) {
	st := NewMemStore()
	ops := NewFakeOps()
	ops.LogFor = func(spec JobSpec) string {
		// The supervisor reports every step's outcome and outputs at the END of
		// the script (all steps share one container, so Go cannot observe them
		// between steps).
		return "##[easydev:result] summary\n" +
			"##[easydev:result] 0 success 0\n" +
			"##[easydev:step-outputs] 0\nversion=1.2.3\n"
	}
	o := NewOrchestrator(st, ops)
	wf, err := ParseWorkflow([]byte(`
on: [rr_opened]
jobs:
  meta:
    runs-on: [linux]
    outputs:
      version: ${{ steps.v.outputs.version }}
    steps:
      - id: v
        run: echo "version=1.2.3"
  publish:
    needs: [meta]
    runs-on: [linux]
    if: ${{ needs.meta.outputs.version != '' }}
    steps:
      - run: echo publishing ${{ needs.meta.outputs.version }}
`))
	if err != nil {
		t.Fatal(err)
	}
	id, err := o.Run(context.Background(), Event{
		Namespace: "team-app", Event: EventRROpened, Ref: "main",
		Head: "sha1", Owner: "alice", Workflow: wf,
	})
	if err != nil {
		t.Fatal(err)
	}
	run := waitTerminal(t, st, id)
	for _, j := range run.Jobs {
		if j.Key == "publish" && j.State != RunSuccess {
			t.Fatalf("publish = %s (%s); outputs did not flow", j.State, j.Error)
		}
	}
}

func waitTerminal(t *testing.T, s Store, id string) *Run {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		r, err := s.GetRun(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if r.State.Terminal() {
			return r
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run %s did not finish", id)
	return nil
}

// decodeStepBodies extracts and decodes every base64 step body the supervisor
// script embeds, so tests can assert on the workflow's actual content.
func decodeStepBodies(t *testing.T, script string) string {
	t.Helper()
	var out strings.Builder
	for _, line := range strings.Split(script, "\n") {
		// printf '%s' '<b64>' | base64 -d > ...
		i := strings.Index(line, "printf '%s' '")
		if i < 0 {
			continue
		}
		rest := line[i+len("printf '%s' '"):]
		j := strings.Index(rest, "'")
		if j < 0 {
			continue
		}
		b, err := base64.StdEncoding.DecodeString(rest[:j])
		if err != nil {
			t.Fatalf("bad base64 step body: %v", err)
		}
		out.WriteString(string(b))
		out.WriteString("\n")
	}
	return out.String()
}
