package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/easydev-org/easydev-platform/internal/ci"
	"github.com/easydev-org/easydev-platform/internal/ops"
)

// TestOpsTenantIsolationOverHTTP is the P1 end-to-end property: two tenants can
// each create a job with the SAME name, get distinct k8s names, and can never
// see or delete each other's job — all over HTTP.
func TestOpsTenantIsolationOverHTTP(t *testing.T) {
	// No k8s in a unit test: the engine is wrapped around a nil JobRunner, so
	// SubmitJob reports "not configured". We exercise the REGISTRY path (list/get/
	// delete isolation) by seeding rows directly.
	reg := ops.NewMemRegistry()
	engine := ops.NewEngine(reg, nil)
	g, err := New(Config{Ops: engine, CI: ci.NewOrchestrator(ci.NewMemStore(), ci.NewFakeOps()), CIStore: ci.NewMemStore()})
	if err != nil {
		t.Fatal(err)
	}

	// Seed a job for team-a straight into the registry (stands in for a k8s job).
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

	// team-a sees its job.
	code, body := do(http.MethodGet, "/api/v1/team-a/ops/jobs")
	if code != 200 || !strings.Contains(body, `"build"`) {
		t.Fatalf("team-a list: %d %s", code, body)
	}

	// team-b sees NOTHING.
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

	// team-b cannot GET team-a's job (404, not 403: existence never leaks).
	code, _ = do(http.MethodGet, "/api/v1/team-b/ops/jobs/build")
	if code != http.StatusNotFound {
		t.Fatalf("cross-tenant get = %d, want 404", code)
	}

	// team-b cannot DELETE it either.
	code, _ = do(http.MethodDelete, "/api/v1/team-b/ops/jobs/build")
	if code != http.StatusNotFound {
		t.Fatalf("cross-tenant delete = %d, want 404", code)
	}

	// team-a's job is intact.
	code, _ = do(http.MethodGet, "/api/v1/team-a/ops/jobs/build")
	if code != http.StatusOK {
		t.Fatalf("owner get = %d", code)
	}
}

// TestCITenantIsolationOverHTTP: a run created for team-a is invisible to team-b
// and a body naming another tenant is rejected.
func TestCITenantIsolationOverHTTP(t *testing.T) {
	store := ci.NewMemStore()
	orch := ci.NewOrchestrator(store, ci.NewFakeOps())
	g, err := New(Config{CI: orch, CIStore: store})
	if err != nil {
		t.Fatal(err)
	}
	yaml := "version: 1\non: [rr_opened]\njobs:\n  t:\n    image: busybox\n    commands: [\"true\"]\n"
	body := `{"trigger":"rr_opened","ref":"main","head":"deadbeef","rr":1,"yaml":` + jsonString(yaml) + `}`

	req := httptest.NewRequest(http.MethodPost, "/api/v1/team-a/ci/events", strings.NewReader(body))
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("trigger = %d %s", rec.Code, rec.Body.String())
	}

	// team-a lists 1 run; team-b lists 0.
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

	// A body naming another tenant is rejected.
	bad := `{"namespace":"team-b","trigger":"rr_opened","head":"x","yaml":` + jsonString(yaml) + `}`
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/team-a/ci/events", strings.NewReader(bad))
	rec2 := httptest.NewRecorder()
	g.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("cross-tenant body = %d, want 403", rec2.Code)
	}
}

// TestVCSRoutingIsClaimedBeforeCatchAll pins D1-3: the platform claims /ops and
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
