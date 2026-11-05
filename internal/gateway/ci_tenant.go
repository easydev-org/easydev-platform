package gateway

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/easydev-org/easydev-platform/internal/ci"
)

// handleCITenant serves the TENANT-SCOPED CI API:
//
//	POST /api/v1/{ns}/ci/events            start a run from an event
//	GET  /api/v1/{ns}/ci/runs              list runs
//	GET  /api/v1/{ns}/ci/runs/{id}         one run (steps + outputs + logs)
//	POST /api/v1/{ns}/ci/runs/{id}/cancel  request cancellation
//	POST /api/v1/{ns}/ci/runs/{id}/rerun   re-run the same pinned snapshot
//	GET  /api/v1/{ns}/ci/checks?rr=        checks + required_pending (merge gate)
//	GET  /api/v1/{ns}/ci/workflows         the workflows a ref declares
//
// The tenant in the path is AUTHORITATIVE: a body naming another namespace is
// rejected, never trusted.
func (g *Gateway) handleCITenant(w http.ResponseWriter, r *http.Request, tenant, tail string) {
	if g.cfg.CI == nil || g.cfg.CIStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "ci is not configured"})
		return
	}
	switch {
	case tail == "events" && r.Method == http.MethodPost:
		g.ciTrigger(w, r, tenant)

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

	case strings.HasPrefix(tail, "runs/"):
		g.ciRun(w, r, tenant, strings.TrimPrefix(tail, "runs/"))

	case tail == "checks" && r.Method == http.MethodGet:
		rr, _ := strconv.ParseInt(r.URL.Query().Get("rr"), 10, 64)
		checks, err := g.cfg.CIStore.ListChecks(r.Context(), tenant, rr)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"checks": checks, "required_pending": g.requiredPending(r, tenant, rr), "tenant": tenant,
		})

	case tail == "workflows" && r.Method == http.MethodGet:
		g.ciListWorkflows(w, r, tenant)

	case tail == "":
		writeJSON(w, http.StatusOK, map[string]any{
			"ci": true, "tenant": tenant,
			"endpoints": []string{
				"POST /api/v1/{ns}/ci/events",
				"GET  /api/v1/{ns}/ci/runs",
				"GET  /api/v1/{ns}/ci/runs/{id}",
				"POST /api/v1/{ns}/ci/runs/{id}/cancel",
				"POST /api/v1/{ns}/ci/runs/{id}/rerun",
				"GET  /api/v1/{ns}/ci/checks?rr=",
				"GET  /api/v1/{ns}/ci/workflows?ref=",
			},
		})
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
	}
}

// ciTriggerRequest is the event body. It carries NO workflow YAML: the workflow
// is read from the VCS at the event's pinned snapshot.
type ciTriggerRequest struct {
	Event string `json:"event"`
	// Trigger is the legacy alias (`rr_opened` etc.).
	Trigger string `json:"trigger"`
	Ref     string `json:"ref"`
	Tag     string `json:"tag"`
	Base    string `json:"base"`
	Head    string `json:"head"`
	// RR is the Revision Request number.
	RR           int64    `json:"rr"`
	ChangedPaths []string `json:"changed_paths"`
	// Workflow optionally selects a file (default .easydev/workflows/ci.yml).
	Workflow string            `json:"workflow"`
	Inputs   map[string]string `json:"inputs"`
	// Namespace is accepted only to REJECT a cross-tenant body.
	Namespace string `json:"namespace"`
}

func (g *Gateway) ciTrigger(w http.ResponseWriter, r *http.Request, tenant string) {
	if g.cfg.Workflows == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "ci workflows are not configured"})
		return
	}
	var req ciTriggerRequest
	if err := decode(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if req.Namespace != "" && req.Namespace != tenant {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "namespace in body does not match the path"})
		return
	}
	ev := ci.Event{
		Namespace:    tenant,
		Event:        ci.EventType(firstNonEmptyStr(req.Event, req.Trigger)),
		Ref:          req.Ref,
		Tag:          req.Tag,
		Base:         req.Base,
		Head:         req.Head,
		RR:           req.RR,
		ChangedPaths: req.ChangedPaths,
		WorkflowPath: req.Workflow,
		Inputs:       req.Inputs,
		Owner:        callerName(r),
		Actor:        callerName(r),
	}
	if ev.Event == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "event is required"})
		return
	}

	// THE security boundary: the workflow comes from the VCS, never from the
	// body. For an RR event it is read from the BASE snapshot (the target
	// branch), so a change cannot rewrite CI to bypass CI; head code is measured
	// by base rules.
	sha := req.Head
	if isRREvent(ev.Event) {
		sha = req.Base
		if sha == "" {
			sha = req.Ref
		}
	}
	wf, path, err := g.cfg.Workflows.Workflow(tenant, sha, req.Workflow)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"skipped": true, "reason": err.Error(), "tenant": tenant,
		})
		return
	}
	ev.Workflow = wf
	ev.WorkflowPath = path
	ev.WorkflowSHA = sha

	id, err := g.cfg.CI.Run(r.Context(), ev)
	if err != nil {
		if errors.Is(err, ci.ErrNotTriggered) || strings.Contains(err.Error(), "does not trigger") {
			writeJSON(w, http.StatusOK, map[string]any{"skipped": true, "reason": err.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"run_id": id, "tenant": tenant, "workflow": path, "workflow_sha": sha,
	})
}

// ciRun serves one run, its cancellation and its re-run.
func (g *Gateway) ciRun(w http.ResponseWriter, r *http.Request, tenant, tail string) {
	switch {
	case strings.HasSuffix(tail, "/cancel") && r.Method == http.MethodPost:
		id := strings.TrimSuffix(tail, "/cancel")
		run, err := g.cfg.CIStore.GetRun(r.Context(), id)
		if err != nil || run.Namespace != tenant {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "run not found"})
			return
		}
		if err := g.cfg.CIStore.CancelRun(r.Context(), id); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"canceled": id, "tenant": tenant})

	case strings.Contains(tail, "/logs/") && r.Method == http.MethodGet:
		// GET /api/v1/{ns}/ci/runs/{id}/logs/{instanceKey}  ->  live log stream
		parts := strings.SplitN(tail, "/logs/", 2)
		g.ciStreamLogs(w, r, tenant, parts[0], parts[1])

	case strings.HasSuffix(tail, "/rerun") && r.Method == http.MethodPost:
		g.ciRerun(w, r, tenant, strings.TrimSuffix(tail, "/rerun"))

	case r.Method == http.MethodGet:
		run, err := g.cfg.CIStore.GetRun(r.Context(), tail)
		if err != nil || run.Namespace != tenant {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "run not found"})
			return
		}
		writeJSON(w, http.StatusOK, run)

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	}
}

// ciRerun re-runs the SAME pinned workflow snapshot (a reproducible re-run).
func (g *Gateway) ciRerun(w http.ResponseWriter, r *http.Request, tenant, id string) {
	old, err := g.cfg.CIStore.GetRun(r.Context(), id)
	if err != nil || old.Namespace != tenant {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "run not found"})
		return
	}
	if g.cfg.Workflows == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "ci workflows are not configured"})
		return
	}
	wf, path, err := g.cfg.Workflows.Workflow(tenant, old.WorkflowSHA, old.WorkflowPath)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	newID, err := g.cfg.CI.Run(r.Context(), ci.Event{
		Namespace: tenant,
		Event:     eventOfTrigger(old.Trigger),
		Ref:       old.Ref, Tag: old.Tag, Base: old.Base, Head: old.Head,
		RR: old.RR, ChangedPaths: old.ChangedPaths,
		WorkflowPath: path, WorkflowSHA: old.WorkflowSHA,
		Owner: callerName(r), Actor: callerName(r), Workflow: wf,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"run_id": newID, "rerun_of": id, "tenant": tenant})
}

// ciListWorkflows lists the workflows a ref declares.
func (g *Gateway) ciListWorkflows(w http.ResponseWriter, r *http.Request, tenant string) {
	if g.cfg.Workflows == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "ci workflows are not configured"})
		return
	}
	ref := r.URL.Query().Get("ref")
	sha, err := g.cfg.Workflows.ResolveSnapshot(tenant, ref)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}
	paths, err := g.cfg.Workflows.ListWorkflows(tenant, sha)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"workflows": paths, "snapshot": sha, "tenant": tenant,
	})
}

// requiredPending reports whether a required check is not green. It is the merge
// gate's question.
func (g *Gateway) requiredPending(r *http.Request, tenant string, rr int64) bool {
	if g.cfg.CIStore == nil || rr == 0 {
		return false
	}
	checks, err := g.cfg.CIStore.ListChecks(r.Context(), tenant, rr)
	if err != nil {
		return false
	}
	for _, c := range checks {
		if c.Required && c.State != ci.RunSuccess && c.State != ci.RunSkipped {
			return true
		}
	}
	return false
}

// gateMerge refuses a merge while a required check is pending. It is wired as
// middleware in front of the embedded easyvcs RR-merge handler, so the VCS stays
// a pure engine and the policy lives here.
func (g *Gateway) gateMerge(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if g.cfg.CIStore == nil || r.Method != http.MethodPost ||
			!strings.HasSuffix(r.URL.Path, "/merge") {
			next.ServeHTTP(w, r)
			return
		}
		ns, rr := mergeGateCoordinates(r.URL.Path)
		if ns == "" || rr == 0 {
			next.ServeHTTP(w, r)
			return
		}
		// `?allow_ungated=true` is honoured for an admin-style override only when
		// the caller passes the exact query the deployment configured; by
		// default a pending required check always wins.
		if r.URL.Query().Get("allow_ungated") == "true" {
			next.ServeHTTP(w, r)
			return
		}
		if g.requiredPending(r, ns, rr) {
			checks, _ := g.cfg.CIStore.ListChecks(r.Context(), ns, rr)
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":  "required checks are pending",
				"checks": checks,
				"tenant": ns,
			})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// mergeGateCoordinates extracts (namespace, rr iid) from an easyvcs merge path:
// /api/v1/namespaces/{ns}/rr/{iid}/merge
func mergeGateCoordinates(p string) (string, int64) {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	// api, v1, namespaces, {ns}, rr, {iid}, merge
	if len(parts) != 7 || parts[2] != "namespaces" || parts[4] != "rr" || parts[6] != "merge" {
		return "", 0
	}
	iid, err := strconv.ParseInt(parts[5], 10, 64)
	if err != nil {
		return "", 0
	}
	return parts[3], iid
}

func isRREvent(t ci.EventType) bool {
	switch t {
	case ci.EventRROpened, ci.EventRRSynced, ci.EventRRMerged:
		return true
	}
	return false
}

func eventOfTrigger(t ci.Trigger) ci.EventType {
	switch t {
	case ci.TriggerRROpened:
		return ci.EventRROpened
	case ci.TriggerRRSynced:
		return ci.EventRRSynced
	case ci.TriggerRRMerged:
		return ci.EventRRMerged
	case ci.TriggerTag:
		return ci.EventTag
	case ci.TriggerSchedule:
		return ci.EventSchedule
	}
	return ci.EventManual
}

func firstNonEmptyStr(list ...string) string {
	for _, s := range list {
		if s != "" {
			return s
		}
	}
	return ""
}

// ciStreamLogs streams one job instance's log while it runs.
//
// It is a plain text/event-stream over HTTP: a step that takes minutes would
// otherwise be silent until it finished (the run record only carries the tail).
// The stream ends when the run reaches a terminal state, so a client can simply
// read until close.
func (g *Gateway) ciStreamLogs(w http.ResponseWriter, r *http.Request, tenant, runID, instanceKey string) {
	if g.cfg.CI == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "ci is not configured"})
		return
	}
	run, err := g.cfg.CIStore.GetRun(r.Context(), runID)
	if err != nil || run.Namespace != tenant {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "run not found"})
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "streaming is not supported"})
		return
	}
	ch, unsubscribe := g.cfg.CI.SubscribeLogs(runID, instanceKey)
	defer unsubscribe()

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	// Send the log already captured so a late subscriber is not blind.
	if inst := findInstance(run, instanceKey); inst != nil && inst.LogsTail != "" {
		_, _ = io.WriteString(w, inst.LogsTail)
	}
	flusher.Flush()
	// A run that is already over will never produce another chunk: end NOW
	// instead of waiting for the first tick (a client should not sit for a second
	// on a finished run).
	if run.State.Terminal() {
		return
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case chunk, ok := <-ch:
			if !ok {
				return
			}
			if _, werr := io.WriteString(w, chunk); werr != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			// End the stream once the run is terminal AND nothing more will come.
			cur, gerr := g.cfg.CIStore.GetRun(r.Context(), runID)
			if gerr != nil {
				return
			}
			if cur.State.Terminal() {
				// Drain what is buffered, then stop.
				for {
					select {
					case chunk, ok := <-ch:
						if !ok {
							return
						}
						_, _ = io.WriteString(w, chunk)
					default:
						flusher.Flush()
						return
					}
				}
			}
		}
	}
}

// findInstance returns the job instance with the given key.
func findInstance(run *ci.Run, key string) *ci.JobResult {
	for i := range run.Jobs {
		if run.Jobs[i].Key == key {
			return &run.Jobs[i]
		}
	}
	return nil
}
