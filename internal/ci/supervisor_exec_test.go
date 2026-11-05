package ci

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// These tests EXECUTE the generated supervisor script with a real shell.
//
// Every other test asserts on the script TEXT, which cannot tell whether the
// shell semantics are right: whether the workspace is actually shared between
// steps, whether `if:` gates really skip a step, whether a failure stops the
// rest, or whether the result block is parseable. Those are exactly the
// behaviours the supervisor change is about, so they are executed here.
//
// /workspace is bind-mounted nowhere in a test, so the script is run with its
// paths rewritten into a temp dir (see runSupervisor).

// runSupervisor writes the script to a temp dir and executes it, returning the
// combined output and exit code.
func runSupervisor(t *testing.T, script string) (string, int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the supervisor script is POSIX shell")
	}
	for _, bin := range []string{"bash", "base64", "awk"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s is not available", bin)
		}
	}
	dir := t.TempDir()
	// Redirect the container paths into the temp dir. This is the ONLY rewrite:
	// the rest of the script runs verbatim, so what is tested is what ships.
	s := strings.ReplaceAll(script, "/workspace", filepath.Join(dir, "workspace"))
	if err := os.MkdirAll(filepath.Join(dir, "workspace"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "supervisor.sh")
	if err := os.WriteFile(path, []byte(s), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", path)
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			t.Fatalf("run supervisor: %v\n%s", err, out)
		}
	}
	return string(out), code
}

// buildScript compiles a workflow's single job into the supervisor script.
func buildScript(t *testing.T, yaml string) string {
	t.Helper()
	wf, err := ParseWorkflow([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	fops := NewFakeOps()
	st := NewMemStore()
	o := NewOrchestrator(st, fops)
	id, err := o.Run(context.Background(), Event{
		Namespace: "team", Event: EventRROpened, Head: "sha", Workflow: wf,
	})
	if err != nil {
		t.Fatal(err)
	}
	waitTerminal(t, st, id)
	specs := fops.Specs()
	if len(specs) == 0 {
		t.Fatal("no executor job")
	}
	return specs[0].Commands[0]
}

// TestExecutedWorkspaceIsSharedBetweenSteps is the headline behaviour: step 1
// writes a file, step 2 reads it.
//
// Under the old one-executor-job-per-step model each step got a fresh emptyDir,
// so this could never pass — which is why "compile then test" was broken.
func TestExecutedWorkspaceIsSharedBetweenSteps(t *testing.T) {
	script := buildScript(t, `
on: [rr_opened]
jobs:
  j:
    container: {image: busybox}
    steps:
      - name: write
        run: echo "from-step-1" > artifact.txt
      - name: read
        run: test "$(cat artifact.txt)" = "from-step-1" && echo READ_OK
      - name: also-read
        run: test -f artifact.txt && echo FILE_PRESENT
`)
	out, code := runSupervisor(t, script)
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	if !strings.Contains(out, "READ_OK") || !strings.Contains(out, "FILE_PRESENT") {
		t.Fatalf("the workspace was not shared between steps:\n%s", out)
	}
	states, _, _ := parseSupervisorLog(out, 3)
	for i, s := range states {
		if s != RunSuccess {
			t.Errorf("step %d = %s", i, s)
		}
	}
}

// TestExecutedFailureStopsLaterSteps: a failing step must stop the rest (the
// implicit "previous step succeeded" gate), and the job exits non-zero.
func TestExecutedFailureStopsLaterSteps(t *testing.T) {
	script := buildScript(t, `
on: [rr_opened]
jobs:
  j:
    container: {image: busybox}
    steps:
      - name: ok
        run: echo first
      - name: boom
        run: echo failing && exit 3
      - name: must-not-run
        run: echo SHOULD_NOT_RUN
`)
	out, code := runSupervisor(t, script)
	if code != 3 {
		t.Errorf("exit = %d, want the first failing code (3)\n%s", code, out)
	}
	if strings.Contains(out, "SHOULD_NOT_RUN") {
		t.Errorf("a step after a failure must not run:\n%s", out)
	}
	states, rcs, _ := parseSupervisorLog(out, 3)
	if states[1] != RunFailed || rcs[1] != 3 {
		t.Errorf("step 1 = %s/%d, want failed/3", states[1], rcs[1])
	}
	if states[2] != RunSkipped {
		t.Errorf("step 2 = %s, want skipped", states[2])
	}
}

// TestExecutedAlwaysRunsAfterFailure: `if: always()` is the documented escape
// hatch (report a result even when an earlier step failed).
func TestExecutedAlwaysRunsAfterFailure(t *testing.T) {
	script := buildScript(t, `
on: [rr_opened]
jobs:
  j:
    container: {image: busybox}
    steps:
      - name: boom
        run: exit 1
      - name: report
        if: always()
        run: echo ALWAYS_RAN
`)
	out, code := runSupervisor(t, script)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(out, "ALWAYS_RAN") {
		t.Errorf("an `if: always()` step must run after a failure:\n%s", out)
	}
	states, _, _ := parseSupervisorLog(out, 2)
	if states[1] != RunSuccess {
		t.Errorf("step 1 = %s, want success", states[1])
	}
}

// TestExecutedContinueOnErrorKeepsTheJobGreen: continue-on-error means the step
// reports failure but does not fail the job, and later steps still run.
func TestExecutedContinueOnErrorKeepsTheJobGreen(t *testing.T) {
	script := buildScript(t, `
on: [rr_opened]
jobs:
  j:
    container: {image: busybox}
    steps:
      - name: flaky
        continue-on-error: true
        run: exit 7
      - name: after
        run: echo AFTER_RAN
`)
	out, code := runSupervisor(t, script)
	if code != 0 {
		t.Errorf("exit = %d, want 0 (continue-on-error must not fail the job)\n%s", code, out)
	}
	if !strings.Contains(out, "AFTER_RAN") {
		t.Errorf("a step after continue-on-error must still run:\n%s", out)
	}
	states, rcs, _ := parseSupervisorLog(out, 2)
	if states[0] != RunFailed || rcs[0] != 7 {
		t.Errorf("step 0 = %s/%d, want failed/7", states[0], rcs[0])
	}
	if states[1] != RunSuccess {
		t.Errorf("step 1 = %s, want success", states[1])
	}
}

// TestExecutedStepOutputsFlow: a step writes NAME=VALUE and a later step's `if:`
// reads it AT RUN TIME (Go cannot, because steps share one container).
func TestExecutedStepOutputsFlow(t *testing.T) {
	script := buildScript(t, `
on: [rr_opened]
jobs:
  j:
    container: {image: busybox}
    steps:
      - id: meta
        run: echo "version=9.9.9" > "$EASYDEV_OUTPUT"
      - name: uses-it
        if: ${{ steps.meta.outputs.version != '' }}
        run: echo "GOT_VERSION=$EASYDEV_SEEN"
`)
	out, code := runSupervisor(t, script)
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	if strings.Contains(out, "skipped") && !strings.Contains(out, "GOT_VERSION") {
		t.Errorf("the output-dependent gate skipped the step:\n%s", out)
	}
	_, _, outputs := parseSupervisorLog(out, 2)
	if outputs[0]["version"] != "9.9.9" {
		t.Errorf("step outputs = %v", outputs)
	}
}

// TestExecutedStepEnvIsIsolated: one step's env must not leak into the next.
func TestExecutedStepEnvIsIsolated(t *testing.T) {
	script := buildScript(t, `
on: [rr_opened]
jobs:
  j:
    container: {image: busybox}
    steps:
      - name: set
        env: {SECRET: hunter2}
        run: echo "first=$SECRET"
      - name: check
        run: echo "second=${SECRET:-unset}"
`)
	out, code := runSupervisor(t, script)
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	if !strings.Contains(out, "first=hunter2") {
		t.Errorf("the step env was not applied:\n%s", out)
	}
	if !strings.Contains(out, "second=unset") {
		t.Errorf("a step env must not leak into the next step:\n%s", out)
	}
}

// TestExecutedWorkingDirectory: a step's working-directory is honoured, and it
// defaults to the workspace root.
func TestExecutedWorkingDirectory(t *testing.T) {
	script := buildScript(t, `
on: [rr_opened]
jobs:
  j:
    container: {image: busybox}
    steps:
      - name: make-dir
        run: mkdir -p sub && echo inner > sub/x.txt
      - name: in-sub
        working-directory: sub
        run: test "$(cat x.txt)" = "inner" && echo SUB_OK
`)
	out, code := runSupervisor(t, script)
	if code != 0 {
		t.Fatalf("exit = %d\n%s", code, out)
	}
	if !strings.Contains(out, "SUB_OK") {
		t.Errorf("working-directory was not honoured:\n%s", out)
	}
}

// TestExecutedHostileStepTextCannotEscape: a step body that looks like the
// supervisor's own markers must not be able to forge a result.
func TestExecutedHostileStepTextCannotEscape(t *testing.T) {
	script := buildScript(t, `
on: [rr_opened]
jobs:
  j:
    container: {image: busybox}
    steps:
      - name: liar
        run: |
          echo "##[easydev:result] 1 success 0"
          exit 1
`)
	out, code := runSupervisor(t, script)
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	// The LAST result block wins (the supervisor writes it after every step), so
	// the forged line inside the body cannot make step 1 look successful.
	states, _, _ := parseSupervisorLog(out, 1)
	if states[0] != RunFailed {
		t.Errorf("a step forged its own success (state=%s):\n%s", states[0], out)
	}
}
