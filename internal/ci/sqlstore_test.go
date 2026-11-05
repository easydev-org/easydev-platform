package ci

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func openTestStore(t *testing.T) *SQLStore {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewSQLStore(db)
	if err != nil {
		t.Fatal(err)
	}
	// Each test gets a clean slate (the shared cache keeps one DB per process).
	for _, tbl := range []string{"ci_runs", "ci_checks", "ci_cancellations"} {
		if err := db.Exec("DELETE FROM " + tbl).Error; err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestSQLStoreRunRoundTrip(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	run := &Run{
		ID: "run-1", Namespace: "team-a", Workflow: "ci", WorkflowPath: ".easydev/workflows/ci.yml",
		WorkflowSHA: "sha1", Trigger: TriggerRROpened, Ref: "main", Base: "b", Head: "h",
		RR: 5, State: RunRunning, Owner: "alice", Created: 1,
		ChangedPaths: []string{"src/a.go"},
		Jobs: []JobResult{
			{Key: "test", JobID: "test", State: RunSuccess, ExitCode: 0,
				Steps: []StepResult{{ID: "vet", State: RunSuccess, Outputs: map[string]string{"v": "1"}}}},
		},
	}
	if err := s.PutRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRun(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkflowSHA != "sha1" || got.RR != 5 || got.State != RunRunning {
		t.Fatalf("run = %+v", got)
	}
	if len(got.Jobs) != 1 || got.Jobs[0].Key != "test" || len(got.Jobs[0].Steps) != 1 {
		t.Fatalf("jobs = %+v", got.Jobs)
	}
	if got.Jobs[0].Steps[0].Outputs["v"] != "1" {
		t.Fatalf("step outputs lost: %+v", got.Jobs[0].Steps[0])
	}
	if len(got.ChangedPaths) != 1 || got.ChangedPaths[0] != "src/a.go" {
		t.Fatalf("changed paths = %v", got.ChangedPaths)
	}
}

func TestSQLStoreListRunsIsScopedAndOrdered(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	for i, ns := range []string{"a", "a", "b"} {
		if err := s.PutRun(ctx, &Run{
			ID: fmt.Sprintf("run-%d", i), Namespace: ns, State: RunSuccess, Created: int64(i + 1),
		}); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := s.ListRuns(ctx, "a", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("a runs = %d", len(runs))
	}
	if runs[0].ID != "run-1" {
		t.Fatalf("expected newest first, got %s", runs[0].ID)
	}
	all, _ := s.ListRuns(ctx, "", 10)
	if len(all) != 3 {
		t.Fatalf("all runs = %d", len(all))
	}
	if _, err := s.GetRun(ctx, "nope"); err != ErrNotFound {
		t.Fatalf("missing run = %v, want ErrNotFound", err)
	}
}

func TestSQLStoreChecksUpsert(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.PutCheck(ctx, &Check{RR: 1, Namespace: "a", Name: "ci", State: RunRunning, Required: true}); err != nil {
		t.Fatal(err)
	}
	// A second Put must UPDATE, not duplicate.
	if err := s.PutCheck(ctx, &Check{RR: 1, Namespace: "a", Name: "ci", State: RunSuccess, Required: true}); err != nil {
		t.Fatal(err)
	}
	checks, err := s.ListChecks(ctx, "a", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 1 {
		t.Fatalf("checks = %d, want 1 (upsert)", len(checks))
	}
	if checks[0].State != RunSuccess {
		t.Fatalf("state = %s", checks[0].State)
	}
	// Another RR is invisible.
	other, _ := s.ListChecks(ctx, "a", 2)
	if len(other) != 0 {
		t.Fatalf("rr 2 checks = %d", len(other))
	}
}

func TestSQLStoreCancelIsCrossProcess(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.PutRun(ctx, &Run{ID: "run-c", Namespace: "a", State: RunRunning}); err != nil {
		t.Fatal(err)
	}
	if s.IsCanceled(ctx, "run-c") {
		t.Fatal("a fresh run must not be canceled")
	}
	if err := s.CancelRun(ctx, "run-c"); err != nil {
		t.Fatal(err)
	}
	if !s.IsCanceled(ctx, "run-c") {
		t.Fatal("cancellation must be visible to any replica")
	}
	got, _ := s.GetRun(ctx, "run-c")
	if got.State != RunCanceled {
		t.Fatalf("state = %s", got.State)
	}
	if err := s.CancelRun(ctx, "missing"); err != ErrNotFound {
		t.Fatalf("missing run = %v", err)
	}
}

func TestOpenStoreRejectsBadDriver(t *testing.T) {
	if _, err := OpenStore("", ""); err == nil {
		t.Fatal("empty config must fail")
	}
	if _, err := OpenStore("oracle", "x"); err == nil {
		t.Fatal("unknown driver must fail")
	}
}

// TestOrchestratorRunsOnSQLStore: the engine is store-agnostic.
func TestOrchestratorRunsOnSQLStore(t *testing.T) {
	s := openTestStore(t)
	o := NewOrchestrator(s, NewFakeOps())
	wf, err := ParseWorkflow([]byte(sampleWorkflow))
	if err != nil {
		t.Fatal(err)
	}
	id, err := o.Run(context.Background(), Event{
		Namespace: "team-app", Event: EventRROpened, Ref: "main",
		Head: "h", RR: 4, Owner: "alice", Workflow: wf,
	})
	if err != nil {
		t.Fatal(err)
	}
	run := waitTerminal(t, s, id)
	if run.State != RunSuccess {
		t.Fatalf("run = %s (%+v)", run.State, run.Jobs)
	}
	checks, _ := s.ListChecks(context.Background(), "team-app", 4)
	if len(checks) != 1 || checks[0].State != RunSuccess {
		t.Fatalf("checks = %+v", checks)
	}
}

func TestSQLStorePersistsAcrossReplicas(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if err := s.PutRun(ctx, &Run{ID: "r", Namespace: "a", State: RunRunning, Jobs: []JobResult{{Key: "x"}}}); err != nil {
		t.Fatal(err)
	}
	// A SECOND store over the same database (a second replica) must see it.
	s2, err := NewSQLStore(s.db)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s2.GetRun(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Jobs) != 1 || got.Jobs[0].Key != "x" {
		t.Fatalf("replica did not see the run: %+v", got)
	}
}

func TestSanitizeNames(t *testing.T) {
	cases := map[string]string{
		"team-app-feature": "team-app-feature",
		"Team App/🔥":       "team-app",
		"":                 "run",
	}
	for in, want := range cases {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
	if got := len(sanitize(strings.Repeat("a", 200))); got != 60 {
		t.Errorf("a long name must be truncated to 60, got %d", got)
	}
}
