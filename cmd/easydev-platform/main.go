// easydev-platform is the COMPLETE platform binary: it embeds easyvcs (as a
// library), proxies easyblobs and easyops, and adds CI. Every component stays
// independently deployable — this binary is one way to compose them.
//
// Flags/env (each component is optional; a missing one 503s its prefix):
//
//	--listen             :8080                    (LISTEN)
//	--vcs-db-driver      sqlite|postgres|mysql    (EASYVCS_DB_DRIVER)
//	--vcs-db-dsn         ...                      (EASYVCS_DB_DSN)
//	--vcs-data           ./data                   (EASYVCS_HOME)
//	--vcs-admin-token    ...                      (EASYVCS_ADMIN_TOKEN)
//	--vcs-url            http://easyvcs           (EASYDEV_VCS_URL: the URL
//	                                               checkouts/actions fetch from;
//	                                               defaults to this server)
//	--blobs-url          http://easyblobs:8080    (EASYDEV_BLOBS_URL, '' = disabled)
//	--ops-url            http://easyops:8080      (EASYDEV_OPS_URL, '' = disabled)
//	--ci-ops             easyops|fake             (EASYDEV_CI_OPS)
//	--gate-merges        true|false               (EASYDEV_GATE_MERGES)
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/easylab-platform/easyvcs/server"

	"github.com/easy-platform/easyops/k8s/jobmgr"
	"github.com/easy-platform/easyops/k8s/k8sconfig"
	"github.com/easy-platform/easyops/k8s/servicesmgr"
	"k8s.io/client-go/kubernetes"

	"github.com/easydev-org/easydev-platform/internal/ci"
	"github.com/easydev-org/easydev-platform/internal/gateway"
	"github.com/easydev-org/easydev-platform/internal/ops"
	"github.com/easydev-org/easydev-platform/internal/tenant"
	"github.com/easydev-org/easydev-platform/internal/wf"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	listen := flag.String("listen", env("LISTEN", ":8080"), "listen address")
	driver := flag.String("vcs-db-driver", env("EASYVCS_DB_DRIVER", "sqlite"), "easyvcs db driver")
	dsn := flag.String("vcs-db-dsn", env("EASYVCS_DB_DSN", ""), "easyvcs db dsn")
	data := flag.String("vcs-data", env("EASYVCS_HOME", "./data"), "easyvcs data dir (sqlite)")
	admin := flag.String("vcs-admin-token", env("EASYVCS_ADMIN_TOKEN", ""), "easyvcs admin token")
	vcsURL := flag.String("vcs-url", env("EASYDEV_VCS_URL", ""),
		"easyvcs base URL used for checkouts/actions ('' = derive from the listen address)")
	blobsURL := flag.String("blobs-url", env("EASYDEV_BLOBS_URL", ""), "easyblobs base url ('' = disabled)")
	opsURL := flag.String("ops-url", env("EASYDEV_OPS_URL", ""), "easyops base url ('' = disabled)")
	ciOps := flag.String("ci-ops", env("EASYDEV_CI_OPS", ""), "ci ops backend: easyops|fake")
	gateMerges := flag.Bool("gate-merges", env("EASYDEV_GATE_MERGES", "true") == "true",
		"refuse an RR merge while a required check is pending")
	k8sNS := flag.String("k8s-namespace", env("EASYDEV_K8S_NAMESPACE", "easydev"),
		"the ONE k8s namespace all tenants share (tenants are isolated by the platform's registry, not by k8s namespaces)")
	flag.Parse()

	if *dsn == "" && *driver == "sqlite" {
		*dsn = *data + "/easyvcs.db"
		if err := os.MkdirAll(*data, 0o755); err != nil {
			log.Fatalf("platform: create data dir: %v", err)
		}
	}

	cs, err := gateway.OpenVCS(*driver, *dsn)
	if err != nil {
		log.Fatalf("platform: open easyvcs store: %v", err)
	}
	// Bootstrap: the admin token creates the "admin" user (owner of nothing yet)
	// with a write token of that plaintext, so a fresh platform is usable.
	if *admin != "" {
		if u, uerr := cs.CreateUser("admin", "Administrator"); uerr == nil && u != nil {
			if _, terr := cs.CreateToken(*admin, u.ID, "write"); terr != nil {
				log.Printf("platform: admin token: %v", terr)
			}
		}
	}
	vcs := server.New(cs, nil)
	// Let the platform decide identity. easyvcs keeps its default token path
	// whenever the resolver declines (anonymous / unknown user), so the
	// standalone behaviour is preserved.
	ident := &tenant.Resolver{CS: cs}
	vcs.WithIdentity(ident.Identity)

	// Workflows/actions are read OUT of the VCS at pinned snapshots. Installing
	// the source as the action resolver also makes `uses:` work inside a run.
	wfSource := wf.New(cs)
	ci.SetActionResolver(wfSource)

	// Ops engine: the tenant-safe registry + (optionally) the real k8s backend
	// from easyops' library packages. Tenants share ONE k8s namespace
	// (k8sNS); isolation is enforced by the registry, not by k8s namespaces.
	reg := ops.NewMemRegistry()
	var jobRunner ops.JobRunner
	if *ciOps != "fake" {
		// jobmgr.New builds its clientset from the explicit easyops kubeconfig
		// (or the in-cluster config). A missing cluster is NOT fatal: we degrade
		// to the fake ops backend.
		jm, jerr := jobmgr.New(jobmgr.Config{Namespace: *k8sNS})
		if jerr != nil {
			log.Printf("platform: k8s unavailable (%v); CI falls back to the fake ops backend", jerr)
		} else {
			jobRunner = jm
			log.Printf("platform: CI using REAL k8s Jobs in namespace %q (tenants isolated by the registry)", *k8sNS)
		}
	}
	// The secrets backend shares the same k8s config as the job runner.
	var secretsSvc *ops.SecretsService
	if cfg, cerr := k8sconfig.RESTConfig(); cerr == nil {
		if cs, serr := kubernetes.NewForConfig(cfg); serr == nil {
			secretsSvc = &ops.SecretsService{
				Client: servicesmgr.NewWithClientset(cs, servicesmgr.Config{Namespace: *k8sNS}),
			}
		} else {
			log.Printf("platform: secrets backend unavailable: %v", serr)
		}
	} else {
		log.Printf("platform: secrets backend unavailable: %v", cerr)
	}
	engine := ops.NewEngine(reg, jobRunner)

	// Base URL the job's checkout prefix downloads from. When the platform
	// embeds easyvcs, the checkout can also be served by this very process.
	baseVCS := *vcsURL
	if baseVCS == "" {
		baseVCS = "http://127.0.0.1" + normalizeListen(*listen)
		log.Printf("platform: EASYDEV_VCS_URL unset; checkouts will use %q (set EASYDEV_VCS_URL in a real deployment)", baseVCS)
	}

	// CI: an OpsClient. With a real backend it runs k8s Jobs; otherwise the fake.
	var ciOpsClient ci.OpsClient
	if jobRunner != nil {
		fac := ci.NewEngineOpsFactory(engine)
		fac.VCSBase = baseVCS
		fac.VCSUser = "admin"
		fac.VCSToken = *admin
		ciOpsClient = fac
	} else {
		ciOpsClient = ci.NewFakeOps()
		log.Printf("platform: CI using the in-process fake ops backend")
	}
	ciStore := ci.Store(ci.NewMemStore())
	// Durable CI state: reuse the deployment's database so a run survives a
	// restart. A failure here is not fatal (a single-process deployment keeps the
	// in-memory store), but it is noisy because runs would be lost on restart.
	if *driver == "postgres" || *driver == "mysql" || *driver == "mariadb" {
		if s, serr := ci.OpenStore(*driver, *dsn); serr == nil {
			ciStore = s
			log.Printf("platform: CI state in %s", *driver)
		} else {
			log.Printf("platform: CI falling back to the in-memory store: %v", serr)
		}
	} else {
		if s, serr := ci.OpenStore("sqlite", *data+"/ci.db"); serr == nil {
			ciStore = s
			log.Printf("platform: CI state in sqlite %s", *data+"/ci.db")
		} else {
			log.Printf("platform: CI falling back to the in-memory store: %v", serr)
		}
	}
	// WithVCS tells the in-container supervisor how to fetch the source tree: the
	// checkout now happens ONCE per job (inside the script that runs all of its
	// steps), not once per step.
	orch := ci.NewOrchestrator(ciStore, ciOpsClient).
		WithConcurrency(ci.NewConcurrencyGate(ciStore)).
		WithVCS(baseVCS, "admin", *admin)
	if secretsSvc != nil {
		// `${{ secrets.X }}` becomes a REFERENCE into a per-run Secret, so a
		// value never enters the job spec or a log.
		orch.WithSecrets(secretsSvc, secretsSvc)
	}
	// `permissions:` mints a scoped token from the VCS store. The platform's
	// admin user owns the CI tokens (a run's job is the platform acting for the
	// tenant, and the reverse — a tenant token — is what the GATEWAY injects for
	// tenant-initiated actions).
	if adminUser, uerr := cs.GetUserByUsername("admin"); uerr == nil && adminUser != nil {
		orch.WithTokenMinter(&ops.VCSTokenMinter{CS: cs, UserID: adminUser.ID})
	}
	orch.WithLogCap(8192)

	gw, err := gateway.New(gateway.Config{
		VCS:        vcs,
		BlobsURL:   *blobsURL,
		OpsURL:     *opsURL,
		CI:         orch,
		CIStore:    ciStore,
		Ops:        engine,
		Workflows:  wfSource,
		GateMerges: *gateMerges,
	})
	if err != nil {
		log.Fatalf("platform: gateway: %v", err)
	}

	srv := &http.Server{
		Addr:              *listen,
		Handler:           gw,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Printf("easydev-platform listening on %s (vcs embedded; blobs=%q ops=%q ci-gate=%v)",
			*listen, *blobsURL, *opsURL, *gateMerges)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("platform: serve: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	log.Printf("easydev-platform stopped")
}

// normalizeListen turns a listen address into a host:port suitable for a URL
// (":8080" -> ":8080", "0.0.0.0:8080" -> ":8080").
func normalizeListen(addr string) string {
	if strings.HasPrefix(addr, "0.0.0.0") {
		return strings.TrimPrefix(addr, "0.0.0.0")
	}
	return addr
}
