// easydev-platform is the COMPLETE platform binary: it embeds easyvcs (as a
// library), proxies easyblobs and easyops, and adds CI. Every component stays
// independently deployable — this binary is one way to compose them.
//
// Flags/env (each component is optional; a missing one 503s its prefix):
//
//	--listen             :8080
//	--vcs-db-driver      sqlite|postgres|mysql   (EASYVCS_DB_DRIVER)
//	--vcs-db-dsn         ...                     (EASYVCS_DB_DSN)
//	--vcs-data           ./data                  (sqlite dir + blobs)
//	--vcs-admin-token    ...                     (EASYVCS_ADMIN_TOKEN)
//	--blobs-url          http://easyblobs:8080   (empty = disabled)
//	--ops-url            http://easyops:8080     (empty = disabled)
//	--ci-ops             easyops|fake            (default fake when --ops-url empty)
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/easylab-platform/easyvcs/server"

	"github.com/easy-platform/easyops/k8s/jobmgr"

	"github.com/easydev-org/easydev-platform/internal/ci"
	"github.com/easydev-org/easydev-platform/internal/gateway"
	"github.com/easydev-org/easydev-platform/internal/ops"
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
	blobsURL := flag.String("blobs-url", env("EASYDEB_BLOBS_URL", env("EASYDEV_BLOBS_URL", "")), "easyblobs base url ('' = disabled)")
	opsURL := flag.String("ops-url", env("EASYDEV_OPS_URL", ""), "easyops base url ('' = disabled)")
	ciOps := flag.String("ci-ops", env("EASYDEV_CI_OPS", ""), "ci ops backend: easyops|fake")
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

	// Ops engine: the tenant-safe registry + (optionally) the real k8s backend
	// from easyops' library packages. Tenants share ONE k8s namespace
	// (k8sNS); isolation is enforced by the registry, not by k8s namespaces.
	reg := ops.NewMemRegistry()
	var jobRunner ops.JobRunner
	if *ciOps != "fake" {
		// NEWWithClientset needs a clientset; New() builds it from in-cluster
		// config. A missing cluster is NOT fatal: we degrade to fake.
		jm, jerr := jobmgr.New(jobmgr.Config{Namespace: *k8sNS})
		if jerr != nil {
			log.Printf("platform: k8s unavailable (%v); CI falls back to the fake ops backend", jerr)
		} else {
			jobRunner = jm
			log.Printf("platform: CI using REAL k8s Jobs in namespace %q (tenants isolated by the registry)", *k8sNS)
		}
	}
	engine := ops.NewEngine(reg, jobRunner)

	// CI: an OpsClient. With a real backend it runs k8s Jobs; otherwise the fake.
	var ciOpsClient ci.OpsClient
	if jobRunner != nil {
		// The tenant is per-RUN, so the adapter is created inside the orchestrator
		// hook (see Orchestrator.WithOpsFactory). For the single-tenant default we
		// bind it lazily per run.
		ciOpsClient = ci.NewEngineOpsFactory(engine)
	} else {
		ciOpsClient = ci.NewFakeOps()
		log.Printf("platform: CI using the in-process fake ops backend")
	}
	ciStore := ci.NewMemStore()
	orch := ci.NewOrchestrator(ciStore, ciOpsClient)

	gw, err := gateway.New(gateway.Config{
		VCS:      vcs,
		BlobsURL: *blobsURL,
		OpsURL:   *opsURL,
		CI:       orch,
		CIStore:  ciStore,
		Ops:      engine,
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
		log.Printf("easydev-platform listening on %s (vcs embedded; blobs=%q ops=%q)", *listen, *blobsURL, *opsURL)
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
