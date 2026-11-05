package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/easydev-org/easydev-platform/internal/ci"
)

// handleCI serves the legacy (non-tenant) CI surface under /ci/.
//
// It is kept for a single-tenant deployment and for the web UI's global links.
// The TENANT-scoped API (/api/v1/{ns}/ci/...) is the real surface: it is the one
// that can resolve a workflow from the VCS and enforce merge gating.
//
//	GET /ci/runs?namespace=     list runs
//	GET /ci/runs/{id}           one run
//	GET /ci/checks?namespace=&rr=
//
// There is deliberately NO yaml-accepting trigger here: a workflow always comes
// from the VCS, so the only way to start a run is the tenant-scoped events route.
func (g *Gateway) handleCI(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/ci"), "/")

	switch {
	case path == "/runs" && r.Method == http.MethodGet:
		g.ciListRuns(w, r)
	case strings.HasPrefix(path, "/runs/") && r.Method == http.MethodGet:
		g.ciGetRun(w, r, strings.TrimPrefix(path, "/runs/"))
	case path == "/checks" && r.Method == http.MethodGet:
		g.ciChecks(w, r)
	case path == "" || path == "/":
		writeJSON(w, http.StatusOK, map[string]any{
			"ci": true,
			"endpoints": []string{
				"POST /api/v1/{ns}/ci/events",
				"GET  /api/v1/{ns}/ci/runs",
				"GET  /api/v1/{ns}/ci/runs/{id}",
				"POST /api/v1/{ns}/ci/runs/{id}/cancel",
				"POST /api/v1/{ns}/ci/runs/{id}/rerun",
				"GET  /api/v1/{ns}/ci/checks?rr=",
				"GET  /api/v1/{ns}/ci/workflows?ref=",
				"GET  /ci/runs?namespace=",
				"GET  /ci/checks?namespace=&rr=",
			},
		})
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
	}
}

func (g *Gateway) ciListRuns(w http.ResponseWriter, r *http.Request) {
	if g.cfg.CIStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "ci is not configured"})
		return
	}
	ns := r.URL.Query().Get("namespace")
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	runs, err := g.cfg.CIStore.ListRuns(r.Context(), ns, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (g *Gateway) ciGetRun(w http.ResponseWriter, r *http.Request, id string) {
	if g.cfg.CIStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "ci is not configured"})
		return
	}
	run, err := g.cfg.CIStore.GetRun(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "run not found"})
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (g *Gateway) ciChecks(w http.ResponseWriter, r *http.Request) {
	if g.cfg.CIStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "ci is not configured"})
		return
	}
	ns := r.URL.Query().Get("namespace")
	rr, _ := strconv.ParseInt(r.URL.Query().Get("rr"), 10, 64)
	checks, err := g.cfg.CIStore.ListChecks(r.Context(), ns, rr)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"checks": checks, "required_pending": g.requiredPending(r, ns, rr),
	})
}

// decode reads a bounded JSON body.
func decode(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	return json.Unmarshal(body, v)
}

// writeJSON writes a JSON response.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

var _ = ci.RunSuccess
