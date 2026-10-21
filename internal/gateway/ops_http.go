package gateway

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/easydev-org/easydev-platform/internal/ops"
)

// handleOps serves the tenant-scoped ops API:
//
//	GET    /api/v1/{ns}/ops/jobs              list the tenant's jobs
//	POST   /api/v1/{ns}/ops/jobs              {name, image, commands, env, cpu, memory, timeout_seconds}
//	GET    /api/v1/{ns}/ops/jobs/{name}       one job (+ live k8s state)
//	GET    /api/v1/{ns}/ops/jobs/{name}/logs  log tail
//	DELETE /api/v1/{ns}/ops/jobs/{name}
//
// `{ns}` is the TENANT. Every call is scoped to it: the registry lookup is the
// authority, so another tenant's job is a 404 (never 403), and existence never
// leaks.
func (g *Gateway) handleOps(w http.ResponseWriter, r *http.Request) {
	// Path: /api/v1/<ns>/ops/<rest>
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/")
	parts := strings.SplitN(rest, "/", 3) // [ns, "ops", "<tail>"]
	if len(parts) < 2 || parts[1] != "ops" {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	tenant := parts[0]
	tail := ""
	if len(parts) == 3 {
		tail = parts[2]
	}
	if tenant == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "namespace is required"})
		return
	}
	if g.cfg.Ops == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "ops is not configured"})
		return
	}

	switch {
	case tail == "jobs" && r.Method == http.MethodGet:
		g.opsListJobs(w, r, tenant)
	case tail == "jobs" && r.Method == http.MethodPost:
		g.opsCreateJob(w, r, tenant)
	case strings.HasPrefix(tail, "jobs/"):
		g.opsJob(w, r, tenant, strings.TrimPrefix(tail, "jobs/"))
	case tail == "" || tail == "jobs/":
		writeJSON(w, http.StatusOK, map[string]any{
			"ops": true, "tenant": tenant,
			"endpoints": []string{
				"GET|POST /api/v1/{ns}/ops/jobs",
				"GET|DELETE /api/v1/{ns}/ops/jobs/{name}",
				"GET /api/v1/{ns}/ops/jobs/{name}/logs",
			},
		})
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
	}
}

func (g *Gateway) opsListJobs(w http.ResponseWriter, r *http.Request, tenant string) {
	rows, err := g.cfg.Ops.ListJobs(r.Context(), tenant)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jobs": rows, "tenant": tenant})
}

func (g *Gateway) opsCreateJob(w http.ResponseWriter, r *http.Request, tenant string) {
	var req struct {
		Name           string            `json:"name"`
		Image          string            `json:"image"`
		Commands       []string          `json:"commands"`
		Env            map[string]string `json:"env"`
		CPU            string            `json:"cpu"`
		Memory         string            `json:"memory"`
		TimeoutSeconds int               `json:"timeout_seconds"`
	}
	if err := decode(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if req.Name == "" || req.Image == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "name and image are required"})
		return
	}
	rec, err := g.cfg.Ops.SubmitJob(r.Context(), tenant, ops.JobSpec{
		Name: req.Name, Image: req.Image, Commands: req.Commands, Env: req.Env,
		CPU: req.CPU, Memory: req.Memory, TimeoutSeconds: req.TimeoutSeconds,
		Owner: callerName(r),
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, rec)
}

func (g *Gateway) opsJob(w http.ResponseWriter, r *http.Request, tenant, name string) {
	if strings.HasSuffix(name, "/logs") {
		job := strings.TrimSuffix(name, "/logs")
		tail, _ := strconv.ParseInt(r.URL.Query().Get("tail"), 10, 64)
		if tail <= 0 {
			tail = 200
		}
		logs, err := g.cfg.Ops.JobLogs(r.Context(), tenant, job, tail)
		if err != nil {
			writeJSON(w, opsErrCode(err), map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"logs": logs, "job": job, "tenant": tenant})
		return
	}
	switch r.Method {
	case http.MethodGet:
		rec, j, err := g.cfg.Ops.GetJob(r.Context(), tenant, name)
		// Order matters: ErrNotFound (another tenant's resource) is a 404 even
		// when the k8s backend is unavailable, so existence never leaks.
		if errors.Is(err, ops.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}
		if err != nil {
			writeJSON(w, opsErrCode(err), map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"resource": rec, "job": j, "tenant": tenant})
	case http.MethodDelete:
		if err := g.cfg.Ops.DeleteJob(r.Context(), tenant, name); err != nil {
			writeJSON(w, opsErrCode(err), map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": name, "tenant": tenant})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	}
}

// opsErrCode maps an ops error to an HTTP status: another tenant's resource is
// a 404 (existence is never leaked).
func opsErrCode(err error) int {
	if errors.Is(err, ops.ErrNotFound) {
		return http.StatusNotFound
	}
	msg := err.Error()
	if strings.Contains(msg, "not configured") {
		return http.StatusServiceUnavailable
	}
	return http.StatusBadRequest
}

// callerName is a placeholder for the authenticated subject. P1 wires it to the
// platform identity later; until then the header is the seam.
func callerName(r *http.Request) string {
	if v := r.Header.Get("X-Easydev-User"); v != "" {
		return v
	}
	return "anonymous"
}
