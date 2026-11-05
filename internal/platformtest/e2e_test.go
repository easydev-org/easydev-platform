package platformtest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/easylab-platform/easyvcs/object"
	"github.com/easylab-platform/easyvcs/revision"
	"github.com/easylab-platform/easyvcs/store"

	"github.com/easydev-org/easydev-platform/internal/ci"
	"github.com/easydev-org/easydev-platform/internal/wf"
)

// TestEndToEndWorkflowFromVCSToChecks is the horizontal test: it drives a real
// easyvcs store, reads the workflow out of a real snapshot through
// internal/wf, runs it on the fake backend, and asserts the merge-gate check.
//
// It deliberately touches every seam the production path uses:
//
//	store → wf.Workflow → ci.ParseWorkflow → ci.Plan → ci.runInstance → ci.Check
func TestEndToEndWorkflowFromVCSToChecks(t *testing.T) {
	// 1. A namespace with a commit that carries a workflow.
	cs, err := store.OpenDriver(store.DriverConfig{Kind: "sqlite", DSN: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	u, err := cs.CreateUser("alice", "Alice")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := cs.Create(store.NSRef{Name: "team", Owner: u.ID})
	if err != nil {
		t.Fatal(err)
	}
	ws := revision.NewWorkspace(repo)

	base, _, err := ws.CommitFromChanges(object.ID{}, []revision.FileChangeSpec{
		{Path: "README.md", Content: []byte("hello")},
		{Path: ".easydev/workflows/ci.yml", Content: []byte(`
on:
  rr: {types: [opened]}
jobs:
  test:
    container: {image: busybox}
    required: true
    steps:
      - name: echo
        id: meta
        run: echo version=1.2.3
    outputs:
      version: ${{ steps.meta.outputs.version }}
`)},
	}, "base", store.Author{Name: "alice"}, "")
	if err != nil {
		t.Fatal(err)
	}
	_ = repo.PutRef(&store.Ref{Name: "main", Kind: store.RefBranch, Target: base.RevisionID})

	// 2. A HEAD commit (the change under review) that only touches a source file.
	head, _, err := ws.CommitFromChanges(base.RevisionHash, []revision.FileChangeSpec{
		{Path: "main.go", Content: []byte("package main\nfunc main(){}\n")},
	}, "head", store.Author{Name: "alice"}, "")
	if err != nil {
		t.Fatal(err)
	}

	// 3. The platform resolves the workflow from the BASE snapshot (the rule).
	src := wf.New(cs)
	w, path, err := src.Workflow("team", base.RevisionHash.String(), "")
	if err != nil {
		t.Fatalf("resolve workflow: %v", err)
	}
	if path != ci.DefaultWorkflowPath {
		t.Fatalf("path = %q", path)
	}

	// 4. ci.Run it with the fake backend, asserting the CHECKOUT is the head snapshot.
	fops := ci.NewFakeOps()
	fops.LogFor = func(ci.JobSpec) string { return "##[easydev:outputs]\nversion=1.2.3\n" }
	st := ci.NewMemStore()
	orch := ci.NewOrchestrator(st, fops)
	id, err := orch.Run(context.Background(), ci.Event{
		Namespace: "team", Event: ci.EventRROpened, Ref: "main",
		Base: base.RevisionHash.String(), Head: head.RevisionHash.String(),
		RR: 1, Owner: "alice", Workflow: w, WorkflowPath: path,
		WorkflowSHA: base.RevisionHash.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	run := waitTerminal(t, st, id)
	if run.State != ci.RunSuccess {
		t.Fatalf("run = %s (%+v)", run.State, run.Jobs)
	}

	// 5. The step must have been told to check out HEAD, not the base.
	specs := fops.Specs()
	if len(specs) != 1 {
		t.Fatalf("executor calls = %d", len(specs))
	}
	if specs[0].Checkout == nil || specs[0].Checkout.Ref != head.RevisionHash.String() {
		t.Fatalf("checkout = %+v, want head %s", specs[0].Checkout, head.RevisionHash)
	}
	// 6. Job outputs flow through.
	if run.Jobs[0].Outputs["version"] != "1.2.3" {
		t.Fatalf("job outputs = %v", run.Jobs[0].Outputs)
	}
	// 7. The check is green and REQUIRED (it would gate a merge).
	checks, _ := st.ListChecks(context.Background(), "team", 1)
	if len(checks) != 1 || checks[0].State != ci.RunSuccess || !checks[0].Required {
		t.Fatalf("checks = %+v", checks)
	}
	// 8. And the run records the pinned workflow snapshot (reproducibility).
	blob, _ := json.Marshal(run)
	if !strings.Contains(string(blob), base.RevisionHash.String()) {
		t.Fatalf("the run must record the workflow snapshot:\n%s", blob)
	}
}

// TestActionFromVCSRunsInsideAStep wires the action resolver end to end: a
// `uses:` reference is loaded from a subtree and compiled into the step script.
func TestActionFromVCSRunsInsideAStep(t *testing.T) {
	cs, err := store.OpenDriver(store.DriverConfig{Kind: "sqlite", DSN: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := cs.CreateUser("alice", "Alice")
	repo, _ := cs.Create(store.NSRef{Name: "team", Owner: u.ID})
	ws := revision.NewWorkspace(repo)

	snap, _, err := ws.CommitFromChanges(object.ID{}, []revision.FileChangeSpec{
		{Path: ".easydev/workflows/ci.yml", Content: []byte(`
on: [rr_opened]
jobs:
  t:
    container: {image: busybox}
    steps:
      - uses: team/actions/hello@v1
`)},
		{Path: "actions/hello/action.yml", Content: []byte(`
name: hello
inputs:
  who: {default: world}
runs:
  using: shell
  script: echo "hello $INPUT_WHO"
`)},
	}, "seed", store.Author{Name: "alice"}, "")
	if err != nil {
		t.Fatal(err)
	}
	// A tag pins the action.
	if err := repo.PutRef(&store.Ref{Name: "v1", Kind: store.RefTag, Target: snap.RevisionID}); err != nil {
		t.Fatal(err)
	}

	src := wf.New(cs)
	ci.SetActionResolver(src)
	defer ci.SetActionResolver(nil)

	w, _, err := src.Workflow("team", snap.RevisionHash.String(), "")
	if err != nil {
		t.Fatal(err)
	}
	fops := ci.NewFakeOps()
	st := ci.NewMemStore()
	orch := ci.NewOrchestrator(st, fops)
	id, err := orch.Run(context.Background(), ci.Event{
		Namespace: "team", Event: ci.EventRROpened, Ref: "main",
		Head: snap.RevisionHash.String(), Workflow: w, WorkflowPath: ci.DefaultWorkflowPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	run := waitTerminal(t, st, id)
	if run.State != ci.RunSuccess {
		t.Fatalf("run = %s (%+v)", run.State, run.Jobs)
	}
	specs := fops.Specs()
	if len(specs) != 1 {
		t.Fatalf("executor calls = %d", len(specs))
	}
	if !strings.Contains(specs[0].Commands[0], "hello $INPUT_WHO") {
		t.Fatalf("the action script did not run:\n%s", specs[0].Commands[0])
	}
	if specs[0].Env["INPUT_WHO"] != "world" {
		t.Fatalf("action input not defaulted: %v", specs[0].Env)
	}
}

// TestSQLStoreSurvivesProcessRestart proves the "durable" claim: the rows are in
// a FILE, so a new handle over the same path sees them.
func TestSQLStoreSurvivesProcessRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ci.db")

	first, err := ci.OpenStore("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.PutRun(context.Background(), &ci.Run{
		ID: "persisted", Namespace: "team", State: ci.RunSuccess,
		Jobs: []ci.JobResult{{Key: "build", State: ci.RunSuccess}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := first.PutCheck(context.Background(), &ci.Check{
		RR: 1, Namespace: "team", Name: "ci", State: ci.RunSuccess, Required: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the store file was not created: %v", err)
	}

	// A NEW handle (a restarted process).
	second, err := ci.OpenStore("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	run, err := second.GetRun(context.Background(), "persisted")
	if err != nil {
		t.Fatalf("a restarted store lost the run: %v", err)
	}
	if len(run.Jobs) != 1 || run.Jobs[0].Key != "build" {
		t.Fatalf("run = %+v", run)
	}
	checks, _ := second.ListChecks(context.Background(), "team", 1)
	if len(checks) != 1 || !checks[0].Required {
		t.Fatalf("checks = %+v", checks)
	}
}

// waitTerminal polls the store until the run finishes.
func waitTerminal(t *testing.T, s ci.Store, id string) *ci.Run {
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
