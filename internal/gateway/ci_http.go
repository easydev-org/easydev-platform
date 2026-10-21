package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/easydev-org/easydev-platform/internal/ci"
)

// handleCI serves the CI surface:
//
//	POST /ci/events           {namespace, trigger, ref, base, head, rr, owner, yaml}
//	                          -> {run_id}
//	GET  /ci/runs?namespace=  -> {runs:[...]}
//	GET  /ci/runs/{id}        -> a Run
//	GET  /ci/checks?namespace=&rr= -> {checks:[...], required_pending:bool}
//
// The `required_pending` flag is what a merge gate consults: when true, a merge
// must be REFUSED (a required check is not green).
func (g *Gateway) handleCI(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/ci")
	path = strings.TrimSuffix(path, "/")

	switch {
	case path == "/events" && r.Method == http.MethodPost:
		g.ciEvent(w, r)
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
				"POST /ci/events", "GET /ci/runs?namespace=", "GET /ci/runs/{id}",
				"GET /ci/checks?namespace=&rr=",
			},
		})
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
	}
}

func (g *Gateway) ciEvent(w http.ResponseWriter, r *http.Request) {
	if g.cfg.CI == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "ci is not configured"})
		return
	}
	var ev ci.Event
	if err := decode(r, &ev); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if ev.Namespace == "" || ev.Trigger == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "namespace and trigger are required"})
		return
	}
	id, err := g.cfg.CI.Run(r.Context(), ev)
	if err != nil {
		// "does not trigger" is not an error for the caller: no pipeline ran.
		if strings.Contains(err.Error(), "does not trigger") {
			writeJSON(w, http.StatusOK, map[string]any{"skipped": true, "reason": err.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"run_id": id})
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
		if errors.Is(err, ci.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "run not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
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
	// required_pending is the merge gate: a required check that has not passed
	// blocks the merge.
	pending := false
	for _, c := range checks {
		if c.Required && c.State != ci.RunSuccess {
			pending = true
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"checks": checks, "required_pending": pending,
	})
}

func decode(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(body) == 0 {
		return errors.New("empty body")
	}
	return json.Unmarshal(body, v)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
