package ci

import (
	"context"
	"strings"
	"testing"
)

// TestOneExecutorJobPerInstance is the primary regression test for the
// supervisor change.
//
// A step used to be its own executor job, which meant a FRESH EMPTY WORKSPACE per
// step: "compile in step 1, test the artifact in step 2" could not work, every
// step re-downloaded the checkout, and companion services were recreated. Now a
// job instance submits exactly ONE executor job whose script runs every step.
func TestOneExecutorJobPerInstance(t *testing.T) {
	fops := NewFakeOps()
	st := NewMemStore()
	o := NewOrchestrator(st, fops).WithVCS("http://vcs", "u", "t")

	wf, err := ParseWorkflow([]byte(`
on: [rr_opened]
jobs:
  build:
    container: {image: golang:1.27}
    steps:
      - name: compile
        run: go build -o bin/app ./cmd/app
      - name: test
        run: ./bin/app --selftest
      - name: package
        run: tar czf app.tgz bin/app
`))
	if err != nil {
		t.Fatal(err)
	}
	id, err := o.Run(context.Background(), Event{
		Namespace: "team-a", Event: EventRROpened, Ref: "main", Head: "sha1",
		Workflow: wf, WorkflowPath: DefaultWorkflowPath, WorkflowSHA: "sha1",
	})
	if err != nil {
		t.Fatal(err)
	}
	run := waitTerminal(t, st, id)
	if run.State != RunSuccess {
		t.Fatalf("run = %s (%+v)", run.State, run.Jobs)
	}

	specs := fops.Specs()
	if len(specs) != 1 {
		t.Fatalf("executor jobs = %d, want exactly 1 for 3 steps", len(specs))
	}
	// All three steps must be in the ONE script, and the checkout must appear
	// exactly once (not once per step).
	script := specs[0].Commands[0]
	if got := strings.Count(script, "archive/sha1.tar.gz"); got != 1 {
		t.Errorf("checkout appears %d times, want 1", got)
	}
	body := decodeStepBodies(t, script)
	for _, want := range []string{"go build -o bin/app", "./bin/app --selftest", "tar czf app.tgz"} {
		if !strings.Contains(body, want) {
			t.Errorf("step body missing %q", want)
		}
	}
	// The three step results must still be reported individually.
	if len(run.Jobs[0].Steps) != 3 {
		t.Fatalf("step results = %d, want 3", len(run.Jobs[0].Steps))
	}
	for i, s := range run.Jobs[0].Steps {
		if s.State != RunSuccess {
			t.Errorf("step %d (%s) = %s", i, s.Name, s.State)
		}
	}
}

// TestExecutorNamesAreUnique pins the name-collision fix.
//
// Executor names were built as sanitize(run-instance-head-stepIndex), and sanitize
// TRUNCATES to 60 characters — so the step discriminator at the end was cut off
// and every step of a job produced the SAME name. The second step then failed
// with "job already exists" (a fake backend did not enforce uniqueness, so only a
// real cluster or this assertion catches it).
func TestExecutorNamesAreUnique(t *testing.T) {
	fops := NewFakeOps()
	st := NewMemStore()
	o := NewOrchestrator(st, fops)

	// A deliberately long namespace/branch, so truncation would bite.
	wf, err := ParseWorkflow([]byte(`
on: [rr_opened]
jobs:
  a-very-long-job-identifier-for-truncation:
    container: {image: busybox}
    steps:
      - run: echo 1
      - run: echo 2
      - run: echo 3
      - run: echo 4
  another-long-job-identifier-here:
    container: {image: busybox}
    steps:
      - run: echo 5
      - run: echo 6
`))
	if err != nil {
		t.Fatal(err)
	}
	id, err := o.Run(context.Background(), Event{
		Namespace: "a-very-long-namespace-name-that-eats-the-budget",
		Event:     EventRROpened,
		Ref:       "a-very-long-branch-name-also-eats-budget",
		Head:      "0123456789abcdef0123456789abcdef0123456789abcdef",
		Workflow:  wf,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, st, id)

	seen := map[string]bool{}
	for _, s := range fops.Specs() {
		if seen[s.Name] {
			t.Fatalf("duplicate executor job name %q (names would collide on a real cluster)", s.Name)
		}
		seen[s.Name] = true
		if len(s.Name) > 63 {
			t.Errorf("executor name %q is %d chars (k8s caps at 63)", s.Name, len(s.Name))
		}
	}
}

// TestStepIfIsCompiledToAShellGate asserts the runtime-`if:` path: because all
// steps share one container, Go cannot evaluate a step's `if:` between them, so
// it must be compiled into the script.
func TestStepIfIsCompiledToAShellGate(t *testing.T) {
	fops := NewFakeOps()
	st := NewMemStore()
	o := NewOrchestrator(st, fops)

	wf, err := ParseWorkflow([]byte(`
on: [rr_opened]
jobs:
  j:
    container: {image: busybox}
    steps:
      - id: meta
        run: echo v=1
      - name: gated
        if: ${{ steps.meta.outputs.v != '' }}
        run: echo gated-ran
      - name: always
        if: always()
        run: echo always-ran
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
	specs := fops.Specs()
	if len(specs) == 0 {
		t.Fatal("no executor job was submitted")
	}
	script := specs[0].Commands[0]

	// The output-dependent gate must read the step output AT RUN TIME.
	if !strings.Contains(script, "easydev_out 0 'v'") {
		t.Errorf("the gate does not read the step output at run time:\n%s", script)
	}
	// `always()` must compile to an unconditional gate, not "previous success".
	if !strings.Contains(script, "if { true; }; then") {
		t.Errorf("always() did not compile to an unconditional gate:\n%s", script)
	}
	// A step with no `if:` requires the previous ones to have succeeded.
	if !strings.Contains(script, `test "${EASYDEV_FAILED:-0}" = "0"`) {
		t.Errorf("an implicit gate is missing:\n%s", script)
	}
}

// TestSupervisorParseResults round-trips the machine-readable block.
func TestSupervisorParseResults(t *testing.T) {
	logs := strings.Join([]string{
		"some build output",
		"##[easydev:result] summary",
		"##[easydev:result] 0 success 0",
		"##[easydev:result] 1 failed 2",
		"##[easydev:result] 2 skipped 0",
		"##[easydev:step-outputs] 0",
		"version=1.2.3",
		"sha=abc",
		"##[easydev:step-outputs] 1",
		"",
	}, "\n")
	states, rcs, outputs := parseSupervisorLog(logs, 3)
	if states[0] != RunSuccess || states[1] != RunFailed || states[2] != RunSkipped {
		t.Fatalf("states = %v", states)
	}
	if rcs[1] != 2 {
		t.Fatalf("rcs = %v", rcs)
	}
	if outputs[0]["version"] != "1.2.3" || outputs[0]["sha"] != "abc" {
		t.Fatalf("outputs = %v", outputs)
	}
}

// TestSupervisorScriptSurvivesHostileContent: a workflow step may contain ANY
// text, including something that looks like a heredoc terminator or a
// `##[easydev:` marker. The body is base64'd, so neither can escape.
func TestSupervisorScriptSurvivesHostileContent(t *testing.T) {
	fops := NewFakeOps()
	st := NewMemStore()
	o := NewOrchestrator(st, fops)

	hostile := "echo '##[easydev:result] 0 success 0'\nEASYDEV_EOF\necho done"
	wf, err := ParseWorkflow([]byte(`
on: [rr_opened]
jobs:
  j:
    container: {image: busybox}
    steps:
      - run: |
` + indentYAML(hostile, "          ")))
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
	specs := fops.Specs()
	if len(specs) == 0 {
		t.Fatal("no executor job was submitted")
	}
	script := specs[0].Commands[0]
	// The hostile text must NOT appear literally in the script.
	if strings.Contains(script, "##[easydev:result] 0 success 0\n") &&
		!strings.Contains(script, "base64") {
		t.Error("the step body was interpolated literally instead of encoded")
	}
	decoded := decodeStepBodies(t, script)
	if !strings.Contains(decoded, "##[easydev:result] 0 success 0") {
		t.Errorf("the body did not round-trip:\n%s", decoded)
	}
}

func indentYAML(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}
