package gateway

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/easydev-org/easydev-platform/internal/ci"
)

// handleCITenant serves the TENANT-SCOPED CI API (the D1 path shape):
//
//	POST /api/v1/{ns}/ci/events
//	GET  /api/v1/{ns}/ci/runs
//	GET  /api/v1/{ns}/ci/runs/{id}
//	GET  /api/v1/{ns}/ci/checks?rr=
//
// The tenant in the path is AUTHORITATIVE.
func (g *Gateway) handleCITenant(w http.ResponseWriter, r *http.Request, tenant, tail string) {
	if g.cfg.CI == nil || g.cfg.CIStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "ci is not configured"})
		return
	}
	switch {
	case tail == "events" && r.Method == http.MethodPost:
		var ev ci.Event
		if err := decode(r, &ev); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if ev.Namespace != "" && ev.Namespace != tenant {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "namespace in body does not match the path"})
			return
		}
		ev.Namespace = tenant
		if ev.Trigger == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "trigger is required"})
			return
		}
		id, err := g.cfg.CI.Run(r.Context(), ev)
		if err != nil {
			if strings.Contains(err.Error(), "does not trigger") {
				writeJSON(w, http.StatusOK, map[string]any{"skipped": true, "reason": err.Error()})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"run_id": id, "tenant": tenant})

	case tail == "runs" && r.Method == http.MethodGet:
		limit := 50
		if v := r.URL.Query().Get("limit"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				limit = n
			}
		}
		runs, err := g.cfg.CIStore.ListRuns(r.Context(), tenant, limit)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"runs": runs, "tenant": tenant})

	case strings.HasPrefix(tail, "runs/") && r.Method == http.MethodGet:
		id := strings.TrimPrefix(tail, "runs/")
		run, err := g.cfg.CIStore.GetRun(r.Context(), id)
		if err != nil || run.Namespace != tenant {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "run not found"})
			return
		}
		writeJSON(w, http.StatusOK, run)

	case tail == "checks" && r.Method == http.MethodGet:
		rr, _ := strconv.ParseInt(r.URL.Query().Get("rr"), 10, 64)
		checks, err := g.cfg.CIStore.ListChecks(r.Context(), tenant, rr)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		pending := false
		for _, c := range checks {
			if c.Required && c.State != ci.RunSuccess {
				pending = true
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"checks": checks, "required_pending": pending, "tenant": tenant})

	case tail == "":
		writeJSON(w, http.StatusOK, map[string]any{
			"ci": true, "tenant": tenant,
			"endpoints": []string{
				"POST /api/v1/{ns}/ci/events",
				"GET  /api/v1/{ns}/ci/runs",
				"GET  /api/v1/{ns}/ci/runs/{id}",
				"GET  /api/v1/{ns}/ci/checks?rr=",
			},
		})
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
	}
}
