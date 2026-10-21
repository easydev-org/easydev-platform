// Package gateway is the ONE origin that fronts all three core servers plus the
// CI orchestrator, so a browser or a CLI sees a single server:
//
//	/vcs/*     -> easyvcs (embedded, library)   — the VCS + its web UI (/vcs/ui)
//	/blobs/*   -> easyblobs (proxy)             — the package registry
//	/ops/*     -> easyops (proxy)               — sandbox / jobs / builds
//	/ci/*      -> the CI orchestrator (this platform)
//	/api/v1/*  -> easyvcs (kept at the root so existing clients keep working)
//
// Each core server stays independently deployable: this gateway only MOUNTS
// them. When a component is not configured its prefix 503s, so a partial
// deployment still boots.
package gateway

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"github.com/easylab-platform/easyvcs/server"
	"github.com/easylab-platform/easyvcs/store"

	"github.com/easydev-org/easydev-platform/internal/ci"
	"github.com/easydev-org/easydev-platform/internal/ops"
	"github.com/easydev-org/easydev-platform/internal/tenant"
)

// Config wires the gateway.
type Config struct {
	// VCS is the embedded easyvcs server (nil = disabled).
	VCS *server.Server
	// BlobsURL / OpsURL are the upstream easyblobs / easyops base URLs ("" =
	// disabled).
	BlobsURL string
	OpsURL   string
	// CI is the orchestrator (nil = disabled).
	CI *ci.Orchestrator
	// CIStore holds checks; used by the /ci endpoints.
	CIStore ci.Store
	// Ops is the tenant-safe ops engine (nil = disabled).
	Ops *ops.Engine
}

// Gateway is an http.Handler that routes by prefix.
type Gateway struct {
	cfg   Config
	mux   *http.ServeMux
	blobs *httputil.ReverseProxy
	ops   *httputil.ReverseProxy
}

// New builds the gateway.
func New(cfg Config) (*Gateway, error) {
	g := &Gateway{cfg: cfg, mux: http.NewServeMux()}
	var err error
	if cfg.BlobsURL != "" {
		if g.blobs, err = proxy(cfg.BlobsURL, "/blobs"); err != nil {
			return nil, fmt.Errorf("gateway: blobs: %w", err)
		}
	}
	if cfg.OpsURL != "" {
		if g.ops, err = proxy(cfg.OpsURL, "/ops"); err != nil {
			return nil, fmt.Errorf("gateway: ops: %w", err)
		}
	}
	g.routes()
	return g, nil
}

func proxy(raw, prefix string) (*httputil.ReverseProxy, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, err
	}
	rp := httputil.NewSingleHostReverseProxy(u)
	inner := rp.Director
	rp.Director = func(r *http.Request) {
		inner(r)
		// Strip the platform prefix so the upstream keeps its own paths.
		r.URL.Path = strings.TrimPrefix(r.URL.Path, prefix)
		if r.URL.Path == "" {
			r.URL.Path = "/"
		}
		r.Host = u.Host
	}
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, e error) {
		http.Error(w, "upstream unavailable: "+e.Error(), http.StatusBadGateway)
	}
	return rp, nil
}

func (g *Gateway) routes() {
	// PLATFORM-OWNED segments come FIRST: they must never fall through to
	// easyvcs' catch-all git route (GET /api/v1/namespaces/{ns}/{path...}).
	//   /api/v1/<ns>/ops/...      -> the ops engine
	//   /api/v1/<ns>/ci/...       -> the CI orchestrator
	// Everything else under /api/v1/<ns>/... is the VCS.
	g.mux.HandleFunc("/api/v1/", g.platformAPI)
	g.mux.HandleFunc("/ci/", g.handleCI)
	g.mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"component":"easydev-platform"}` + "\n"))
	})
	g.mux.HandleFunc("/", g.index)
}

// platformAPI dispatches /api/v1/<ns>/<segment>/... : platform segments are
// claimed here, everything else is handed to the embedded easyvcs router.
func (g *Gateway) platformAPI(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/v1/")
	parts := strings.SplitN(rest, "/", 2) // [<ns>, "<segment>/..."]
	ns := parts[0]
	seg := ""
	if len(parts) == 2 {
		seg = parts[1]
	}
	switch {
	case ns == "_":
		// GLOBAL (non-tenant) endpoints live under /api/v1/_/ so a tenant named
		// "users" or "whoami" can never shadow them (D1-S2). "_" is a reserved
		// tenant name.
		g.serveGlobal(w, r, seg)
	case ns == "":
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "namespace is required"})
	case strings.HasPrefix(seg, "ops"):
		g.handleOps(w, r)
	case strings.HasPrefix(seg, "ci"):
		g.handleCITenant(w, r, ns, strings.TrimPrefix(strings.TrimPrefix(seg, "ci"), "/"))
	default:
		// VCS: rewrite /api/v1/<ns>/<rest> -> /api/v1/namespaces/<ns>/<rest>.
		g.serveVCS(w, r, ns, seg)
	}
}

// serveGlobal serves the NON-tenant endpoints under /api/v1/_/.
//
// They are easyvcs' own global handlers (users, tokens, whoami, namespaces), so
// we rewrite /api/v1/_/<rest> -> /api/v1/<rest> and let the embedded server
// answer. The leading "_" is a reserved tenant name.
func (g *Gateway) serveGlobal(w http.ResponseWriter, r *http.Request, seg string) {
	if g.cfg.VCS == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "vcs is not configured"})
		return
	}
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/api/v1/"
	if seg != "" {
		r2.URL.Path += seg
	}
	g.cfg.VCS.Router().ServeHTTP(w, r2)
}

// serveVCS rewrites the platform path to the easyvcs path shape and hands the
// request to the embedded easyvcs router.
func (g *Gateway) serveVCS(w http.ResponseWriter, r *http.Request, ns, seg string) {
	if g.cfg.VCS == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "vcs is not configured"})
		return
	}
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/api/v1/namespaces/" + ns
	if seg != "" {
		r2.URL.Path += "/" + seg
	}
	// Tell the identity resolver which tenant this is (the path is now easyvcs'
	// own shape). Always overwrite: a client-supplied value must never survive.
	r2.Header.Set(tenant.TenantHeader, ns)
	g.cfg.VCS.Router().ServeHTTP(w, r2)
}

// legacyRoutes keeps the old (namespaced) mounting available when someone wires
// the gateway WITHOUT the platform dispatcher (kept for the easyvcs-only mode).
func (g *Gateway) legacyRoutes() {
	// easyvcs: its own mux already serves /api/v1/... and /ui/...; mount it at
	// the ROOT so existing easyvcs clients are unaffected, and ALSO under /vcs
	// for a namespaced deployment.
	if g.cfg.VCS != nil {
		vcs := g.cfg.VCS.Router()
		g.mux.Handle("/api/v1/", vcs)
		g.mux.Handle("/ui/", vcs)
		g.mux.Handle("/ui", vcs)
		g.mux.Handle("/vcs/", http.StripPrefix("/vcs", vcs))
	}
	if g.blobs != nil {
		g.mux.Handle("/blobs/", g.blobs)
	}
	if g.ops != nil {
		g.mux.Handle("/ops/", g.ops)
	}
	g.mux.HandleFunc("/ci/", g.handleCI)
	g.mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"component":"easydev-platform"}` + "\n"))
	})
	g.mux.HandleFunc("/", g.index)
}

// index serves a tiny landing page linking the components.
func (g *Gateway) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<!doctype html><meta charset=utf-8><title>easydev platform</title>
<style>body{font:15px/1.6 system-ui;max-width:38rem;margin:4rem auto;padding:0 1rem}
code{background:#f3f3f3;padding:.1em .3em;border-radius:3px}</style>
<h1>easydev platform</h1>
<p>One origin for <b>easyvcs</b> + <b>easyblobs</b> + <b>easyops</b> + CI.</p>
<ul>
<li><a href="/ui/">/ui/</a> — the EasyVCS web UI</li>
<li><code>/api/v1/…</code> — the EasyVCS REST API (also <code>/vcs/api/v1/…</code>)</li>
<li><code>/blobs/…</code> — the package registry (easyblobs)</li>
<li><code>/ops/…</code> — sandboxes, jobs, builds (easyops)</li>
<li><code>/ci/…</code> — CI runs and checks</li>
</ul>`))
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) { g.mux.ServeHTTP(w, r) }

// OpenVCS opens the easyvcs metadata store (sqlite|postgres|mysql).
func OpenVCS(driver, dsn string) (*store.CentralStore, error) {
	return store.OpenDriver(store.DriverConfig{Kind: driver, DSN: dsn})
}

// Shutdown is a no-op hook (the components own their own lifecycles).
func (g *Gateway) Shutdown(ctx context.Context) error { return nil }

// ErrNotConfigured is returned by components that are disabled.
var ErrNotConfigured = errors.New("gateway: component not configured")

// logOnce logs a boot line once (avoids duplicates with log.Fatal paths).
func logOnce(format string, args ...any) { log.Printf(format, args...) }
