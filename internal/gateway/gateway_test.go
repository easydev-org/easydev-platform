package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/easylab-platform/easyvcs/server"
	"github.com/easylab-platform/easyvcs/store"

	"github.com/easydev-org/easydev-platform/internal/ci"
	"github.com/easydev-org/easydev-platform/internal/ops"
)

// fakeWorkflows is an in-memory WorkflowSource.
type fakeWorkflows struct {
	wf   *ci.Workflow
	path string
	err  error
}

func (f *fakeWorkflows) Workflow(ns, sha, path string) (*ci.Workflow, string, error) {
	if f.err != nil {
		return nil, "", f.err
	}
	p := f.path
	if p == "" {
		p = ci.DefaultWorkflowPath
	}
	return f.wf, p, nil
}
func (f *fakeWorkflows) ListWorkflows(ns, sha string) ([]string, error) {
	return []string{ci.DefaultWorkflowPath}, nil
}
func (f *fakeWorkflows) ResolveSnapshot(ns, ref string) (string, error) {
	if ref == "" {
		return "snap-main", nil
	}
	return "snap-" + ref, nil
}
func (f *fakeWorkflows) Load(ref *ci.ActionRef) ([]byte, bool, error) { return nil, false, nil }

func mustWorkflow(t *testing.T, src string) *ci.Workflow {
	t.Helper()
	w, err := ci.ParseWorkflow([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// TestOpsTenantIsolationOverHTTP is the end-to-end property: two tenants can
// each create a job with the SAME name, get distinct k8s names, and can never
// see or delete each other's job — all over HTTP.
func TestOpsTenantIsolationOverHTTP(t *testing.T) {
	reg := ops.NewMemRegistry()
	engine := ops.NewEngine(reg, nil)
	g, err := New(Config{Ops: engine, CI: ci.NewOrchestrator(ci.NewMemStore(), ci.NewFakeOps()), CIStore: ci.NewMemStore()})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := reg.Create(t.Context(), &ops.Resource{
		Namespace: "team-a", Kind: ops.KindJob, Name: "build", Owner: "alice", State: "running",
	}); err != nil {
		t.Fatal(err)
	}

	do := func(method, path string) (int, string) {
		req := httptest.NewRequest(method, path, nil)
		rec := httptest.NewRecorder()
		g.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}

	code, body := do(http.MethodGet, "/api/v1/team-a/ops/jobs")
	if code != 200 || !strings.Contains(body, `"build"`) {
		t.Fatalf("team-a list: %d %s", code, body)
	}

	code, body = do(http.MethodGet, "/api/v1/team-b/ops/jobs")
	if code != 200 {
		t.Fatalf("team-b list: %d %s", code, body)
	}
	var list struct {
		Jobs []map[string]any `json:"jobs"`
	}
	_ = json.Unmarshal([]byte(body), &list)
	if len(list.Jobs) != 0 {
		t.Fatalf("team-b must see no jobs, got %d: %s", len(list.Jobs), body)
	}

	code, _ = do(http.MethodGet, "/api/v1/team-b/ops/jobs/build")
	if code != http.StatusNotFound {
		t.Fatalf("cross-tenant get = %d, want 404", code)
	}

	code, _ = do(http.MethodDelete, "/api/v1/team-b/ops/jobs/build")
	if code != http.StatusNotFound {
		t.Fatalf("cross-tenant delete = %d, want 404", code)
	}

	code, _ = do(http.MethodGet, "/api/v1/team-a/ops/jobs/build")
	if code != http.StatusOK {
		t.Fatalf("owner get = %d", code)
	}
}

// TestCITenantIsolationOverHTTP: a run created for team-a is invisible to team-b
// and a body naming another tenant is rejected.
func TestCITenantIsolationOverHTTP(t *testing.T) {
	st := ci.NewMemStore()
	orch := ci.NewOrchestrator(st, ci.NewFakeOps())
	wf := mustWorkflow(t, "version: 1\non: [rr_opened]\njobs:\n  t:\n    container: {image: busybox}\n    steps: [{run: 'true'}]\n")
	g, err := New(Config{
		CI: orch, CIStore: st,
		Workflows: &fakeWorkflows{wf: wf},
	})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"event":"rr_opened","ref":"main","base":"snap-base","head":"deadbeef","rr":1}`

	req := httptest.NewRequest(http.MethodPost, "/api/v1/team-a/ci/events", strings.NewReader(body))
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("trigger = %d %s", rec.Code, rec.Body.String())
	}

	get := func(path string) string {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		return w.Body.String()
	}
	if b := get("/api/v1/team-a/ci/runs"); !strings.Contains(b, `"runs"`) || strings.Contains(b, `"runs":[]`) {
		t.Fatalf("team-a should have a run: %s", b)
	}
	if b := get("/api/v1/team-b/ci/runs"); !strings.Contains(b, `"runs":[]`) {
		t.Fatalf("team-b must have no runs: %s", b)
	}

	bad := `{"namespace":"team-b","event":"rr_opened","head":"x"}`
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/team-a/ci/events", strings.NewReader(bad))
	rec2 := httptest.NewRecorder()
	g.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("cross-tenant body = %d, want 403", rec2.Code)
	}
}

// TestCITriggerNeverAcceptsYAML is the security property: a body carrying a
// workflow definition is IGNORED (the workflow comes from the VCS).
func TestCITriggerNeverAcceptsYAML(t *testing.T) {
	st := ci.NewMemStore()
	orch := ci.NewOrchestrator(st, ci.NewFakeOps())
	wf := mustWorkflow(t, "version: 1\non: [rr_opened]\njobs:\n  safe:\n    container: {image: busybox}\n    steps: [{run: 'true'}]\n")
	g, err := New(Config{CI: orch, CIStore: st, Workflows: &fakeWorkflows{wf: wf}})
	if err != nil {
		t.Fatal(err)
	}
	// A body that tries to define an evil workflow.
	body := `{"event":"rr_opened","ref":"main","head":"h","rr":2,
		"yaml":"on: [rr_opened]\njobs:\n  evil: {image: attacker, commands: [curl evil]}"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/team-a/ci/events", strings.NewReader(body))
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("trigger = %d %s", rec.Code, rec.Body.String())
	}
	// The run must carry the VCS workflow's name and jobs, not the body's.
	runs, _ := st.ListRuns(t.Context(), "team-a", 10)
	if len(runs) != 1 {
		t.Fatalf("runs = %d", len(runs))
	}
	for _, j := range runs[0].Jobs {
		if j.JobID == "evil" {
			t.Fatal("the body's yaml must never define a job")
		}
	}
}

// TestCITriggerSkippedWhenNoWorkflow: a repository with no workflow file is a
// 200 "skipped", not an error.
func TestCITriggerSkippedWhenNoWorkflow(t *testing.T) {
	st := ci.NewMemStore()
	orch := ci.NewOrchestrator(st, ci.NewFakeOps())
	g, err := New(Config{CI: orch, CIStore: st, Workflows: &fakeWorkflows{err: errNotFoundStub}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/team-a/ci/events",
		strings.NewReader(`{"event":"rr_opened","head":"h"}`))
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"skipped":true`) {
		t.Fatalf("no-workflow = %d %s", rec.Code, rec.Body.String())
	}
}

// TestMergeGateBlocksOnRequiredCheck: a pending required check refuses the merge
// with 409 BEFORE easyvcs is asked.
func TestMergeGateBlocksOnRequiredCheck(t *testing.T) {
	cs, err := store.OpenDriver(store.DriverConfig{Kind: "sqlite", DSN: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	vcs := server.New(cs, nil)
	st := ci.NewMemStore()
	_ = st.PutCheck(t.Context(), &ci.Check{
		RR: 3, Namespace: "team-a", Name: "ci", State: ci.RunRunning, Required: true,
	})
	g, err := New(Config{VCS: vcs, CIStore: st, GateMerges: true})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/team-a/rr/3/merge", nil)
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("merge = %d %s, want 409", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "required checks are pending") {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

// TestMergeGateAllowsWhenGreen: with the required check green the gate steps
// aside (easyvcs then answers on its own — 401 for an unauthenticated caller).
func TestMergeGateAllowsWhenGreen(t *testing.T) {
	cs, err := store.OpenDriver(store.DriverConfig{Kind: "sqlite", DSN: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	vcs := server.New(cs, nil)
	st := ci.NewMemStore()
	_ = st.PutCheck(t.Context(), &ci.Check{
		RR: 3, Namespace: "team-a", Name: "ci", State: ci.RunSuccess, Required: true,
	})
	g, err := New(Config{VCS: vcs, CIStore: st, GateMerges: true})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/team-a/rr/3/merge", nil)
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code == http.StatusConflict {
		t.Fatalf("a green check must not be gated: %s", rec.Body.String())
	}
}

// TestMergeGateCoordinates pins the path parsing.
func TestMergeGateCoordinates(t *testing.T) {
	cases := []struct {
		path string
		ns   string
		rr   int64
	}{
		{"/api/v1/namespaces/team-a/rr/7/merge", "team-a", 7},
		{"/api/v1/team-a/rr/7/merge", "", 0}, // platform shape, not easyvcs'
		{"/api/v1/namespaces/a/rr/x/merge", "", 0},
	}
	for _, c := range cases {
		ns, rr := mergeGateCoordinates(c.path)
		if ns != c.ns || rr != c.rr {
			t.Errorf("%s -> (%q,%d), want (%q,%d)", c.path, ns, rr, c.ns, c.rr)
		}
	}
}

// TestVCSRoutingIsClaimedBeforeCatchAll pins that the platform claims /ops and
// /ci so easyvcs' git catch-all can never swallow them.
func TestVCSRoutingIsClaimedBeforeCatchAll(t *testing.T) {
	reg := ops.NewMemRegistry()
	g, _ := New(Config{Ops: ops.NewEngine(reg, nil), CI: ci.NewOrchestrator(ci.NewMemStore(), ci.NewFakeOps()), CIStore: ci.NewMemStore()})
	for _, p := range []string{"/api/v1/team-a/ops", "/api/v1/team-a/ci"} {
		r := httptest.NewRequest(http.MethodGet, p, nil)
		w := httptest.NewRecorder()
		g.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("%s = %d %s", p, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), `"tenant":"team-a"`) {
			t.Fatalf("%s not handled by the platform: %s", p, w.Body.String())
		}
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

var errNotFoundStub = errNotFound("no workflow in this repo")

type errNotFound string

func (e errNotFound) Error() string { return string(e) }

// TestCILogStreamDeliversChunksAndEnds: the live-log endpoint streams chunks and
// closes once the run is terminal, so a client can read to EOF.
func TestCILogStreamDeliversChunksAndEnds(t *testing.T) {
	st := ci.NewMemStore()
	orch := ci.NewOrchestrator(st, ci.NewFakeOps())
	g, err := New(Config{CI: orch, CIStore: st})
	if err != nil {
		t.Fatal(err)
	}
	// A terminal run with a recorded tail.
	_ = st.PutRun(t.Context(), &ci.Run{
		ID: "run-logs", Namespace: "team-a", State: ci.RunSuccess,
		Jobs: []ci.JobResult{{Key: "build", State: ci.RunSuccess, LogsTail: "tail-before-subscribe\n"}},
	})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/team-a/ci/runs/run-logs/logs/build", nil)
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	// The already-captured tail must be sent first, so a late subscriber is not
	// blind; the handler then returns because the run is terminal.
	if !strings.Contains(rec.Body.String(), "tail-before-subscribe") {
		t.Errorf("body = %q", rec.Body.String())
	}
}

// TestCILogStreamIsTenantScoped: another tenant cannot read it.
func TestCILogStreamIsTenantScoped(t *testing.T) {
	st := ci.NewMemStore()
	orch := ci.NewOrchestrator(st, ci.NewFakeOps())
	g, _ := New(Config{CI: orch, CIStore: st})
	_ = st.PutRun(t.Context(), &ci.Run{ID: "run-x", Namespace: "team-a", State: ci.RunSuccess})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/team-b/ci/runs/run-x/logs/build", nil)
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant log stream = %d, want 404", rec.Code)
	}
}
