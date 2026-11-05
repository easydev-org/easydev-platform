package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"time"

	"github.com/easylab-platform/easyvcs/server"

	"github.com/easydev-org/easydev-platform/internal/ci"
)

// Webhook receiver.
//
// easyvcs POSTs a signed event when an RR opens/syncs/merges or a tag is
// created; this endpoint verifies the signature, resolves the workflow from the
// event's pinned snapshots, and starts a run. It is what removes the manual
// `POST /ci/events` step.
//
// Endpoint: POST /api/v1/{ns}/ci/hooks
//
// The tenant comes from the PATH (authoritative), and must match the event's
// namespace: a receiver that trusted the body would let one tenant trigger
// another's pipelines.

// webhookMaxAge bounds the replay window.
const webhookMaxAge = 5 * time.Minute

// ciWebhook receives one easyvcs event.
func (g *Gateway) ciWebhook(w http.ResponseWriter, r *http.Request, tenant string) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if g.cfg.Workflows == nil || g.cfg.CI == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "ci is not configured"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	// Verify BEFORE parsing: an unsigned request must not even be interpreted.
	if g.cfg.WebhookSecret != "" {
		ts, terr := strconv.ParseInt(r.Header.Get("X-EasyVCS-Timestamp"), 10, 64)
		if terr != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing or invalid timestamp"})
			return
		}
		if verr := server.VerifyWebhook(g.cfg.WebhookSecret, ts, body,
			r.Header.Get("X-EasyVCS-Signature"), webhookMaxAge); verr != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": verr.Error()})
			return
		}
	}

	var ev server.WebhookEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid payload"})
		return
	}
	// The path is authoritative: a body naming another tenant is a forgery, not a
	// routing hint.
	if ev.Namespace != "" && ev.Namespace != tenant {
		writeJSON(w, http.StatusForbidden, map[string]any{
			"error": "namespace in the payload does not match the path",
		})
		return
	}

	trigger, ok := ciEventOfWebhook(ev.Event)
	if !ok {
		// An event we do not act on is NOT an error (the sender may publish more).
		writeJSON(w, http.StatusOK, map[string]any{"ignored": ev.Event})
		return
	}

	// Resolve the workflow from the BASE snapshot for RR events (so a change
	// cannot rewrite CI to bypass CI); a tag event reads the tagged snapshot.
	sha := ev.Base
	ref := ev.Ref
	if ev.Event == "tag.created" {
		sha = ev.Head
		ref = ev.Tag
	}
	wf, path, werr := g.cfg.Workflows.Workflow(tenant, sha, "")
	if werr != nil {
		writeJSON(w, http.StatusOK, map[string]any{"skipped": true, "reason": werr.Error()})
		return
	}

	runID, rerr := g.cfg.CI.Run(r.Context(), ci.Event{
		Namespace:    tenant,
		Event:        trigger,
		Ref:          ref,
		Tag:          ev.Tag,
		Base:         ev.Base,
		Head:         ev.Head,
		RR:           ev.RR,
		ChangedPaths: ev.ChangedPaths,
		Actor:        ev.Actor,
		Owner:        ev.Actor,
		WorkflowPath: path,
		WorkflowSHA:  sha,
		Workflow:     wf,
	})
	if rerr != nil {
		if rerr == ci.ErrNotTriggered {
			writeJSON(w, http.StatusOK, map[string]any{"skipped": true, "reason": rerr.Error()})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": rerr.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"run_id": runID, "tenant": tenant, "event": ev.Event,
	})
}

// ciEventOfWebhook maps a webhook event name onto the CI trigger.
func ciEventOfWebhook(name string) (ci.EventType, bool) {
	switch name {
	case "rr.opened":
		return ci.EventRROpened, true
	case "rr.synced":
		return ci.EventRRSynced, true
	case "rr.merged":
		return ci.EventRRMerged, true
	case "tag.created":
		return ci.EventTag, true
	}
	return "", false
}

// InProcessSink adapts the gateway's webhook HANDLER to easyvcs' WebhookSink.
//
// A single-binary deployment (easyvcs embedded in the platform) should NOT POST
// to itself over the network: that adds a loopback round-trip, a port the process
// must be able to reach, and a failure mode (a misconfigured URL silently stops
// all CI). The sink instead calls the handler directly, so the event path is
// identical but has no transport.
type InProcessSink struct {
	Handler http.Handler
	// Secret signs the synthetic request so the handler's verification is
	// exercised exactly as it is in the networked case.
	Secret string
}

// Deliver implements server.WebhookSink.
func (s *InProcessSink) Deliver(ev server.WebhookEvent) {
	if s.Handler == nil {
		return
	}
	body, err := json.Marshal(ev)
	if err != nil {
		return
	}
	ts := time.Now().Unix()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/"+ev.Namespace+"/ci/hooks", bytes.NewReader(body))
	req.Header.Set("X-EasyVCS-Event", ev.Event)
	req.Header.Set("X-EasyVCS-Timestamp", strconv.FormatInt(ts, 10))
	if s.Secret != "" {
		req.Header.Set("X-EasyVCS-Signature", server.SignWebhook(s.Secret, ts, body))
	}
	rec := httptest.NewRecorder()
	// The delivery is a NOTIFICATION: run it synchronously here so a test (and a
	// single-process deployment) has a deterministic "the run exists" moment.
	s.Handler.ServeHTTP(rec, req)
}
