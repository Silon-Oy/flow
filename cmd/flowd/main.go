// Command flowd is the Flow central service: lease manager, runner registry,
// telemetry sink, GitHub scanner, and the REST + SSE API (§4, §6).
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/Silon-Oy/flow/internal/api"
	"github.com/Silon-Oy/flow/internal/lease"
	"github.com/Silon-Oy/flow/internal/store"
)

func main() {
	addr := envOr("FLOWD_ADDR", ":8080")
	dsn := os.Getenv("FLOW_DATABASE_URL")
	if dsn == "" {
		log.Fatal("flowd: FLOW_DATABASE_URL is required")
	}
	tenantName := envOr("FLOW_BOOTSTRAP_TENANT", "default")
	ghToken := os.Getenv("FLOW_GITHUB_TOKEN") // optional; empty = anon (rate-limited)
	scanInterval := durationOr("FLOW_SCAN_INTERVAL", 60*time.Second)

	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()

	// Self-migrate before serving.
	if err := store.Migrate(dsn); err != nil {
		log.Fatalf("flowd: migrate: %v", err)
	}

	st, err := store.Open(rootCtx, dsn)
	if err != nil {
		log.Fatalf("flowd: open db: %v", err)
	}
	defer st.Close()

	tenantID, err := ensureTenant(rootCtx, st, tenantName)
	if err != nil {
		log.Fatalf("flowd: bootstrap tenant: %v", err)
	}
	log.Printf("flowd: bootstrap tenant %q -> %s", tenantName, tenantID)

	// Optional: promote a named GitHub login to admin in the bootstrap tenant.
	// Empty => skip (a single-user deploy can run as developer-only). This is
	// the only knob that mints an admin without a prior admin to authorise it;
	// admins thereafter manage roles via SQL (or the admin CLI in #9).
	if err := bootstrapAdmin(rootCtx, st, tenantID); err != nil {
		log.Fatalf("flowd: bootstrap admin: %v", err)
	}

	// Optional: bootstrap a single github_app_install row from env. Mirrors
	// the github-app-auth.sh one-triplet model so a single-tenant deploy
	// works out of the box; additional installations are added via SQL
	// until #9 (admin CLI) ships. Empty FLOW_GITHUB_APP_ORG => skip.
	if err := bootstrapAppInstall(rootCtx, st, tenantID); err != nil {
		log.Fatalf("flowd: bootstrap github-app install: %v", err)
	}

	srv := api.New(st.Pool, tenantID)

	// Background lease reaper: expired leases return work to the queue (§5).
	go runReaper(rootCtx, lease.NewManager(st.Pool))

	// Background GitHub scanner: the only GitHub-polling component (§4).
	go api.NewScanner(srv, ghToken, scanInterval).Run(rootCtx)

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("flowd listening on %s", addr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("flowd: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Println("flowd shutting down")
	rootCancel()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		log.Printf("flowd: graceful shutdown failed: %v", err)
	}
}

// bootstrapAdmin upserts an app_user row with role=admin for the GitHub login
// in FLOW_BOOTSTRAP_ADMIN in the bootstrap tenant. Idempotent: an existing
// row is promoted, a missing row is created. Empty env => no-op. Roles are
// enforced by middleware (§7); this knob exists so the first admin can exist
// before any other admin has authorised one.
func bootstrapAdmin(ctx context.Context, st *store.Store, tenantID string) error {
	login := os.Getenv("FLOW_BOOTSTRAP_ADMIN")
	if login == "" {
		return nil
	}
	if _, err := st.Pool.Exec(ctx, `
		INSERT INTO app_user (tenant_id, github_login, role)
		VALUES ($1, $2, 'admin'::user_role)
		ON CONFLICT (tenant_id, github_login) DO UPDATE
		   SET role = 'admin'::user_role`,
		tenantID, login); err != nil {
		return err
	}
	log.Printf("flowd: bootstrap admin: tenant=%s github_login=%s", tenantID, login)
	return nil
}

// ensureTenant returns the id of the bootstrap tenant, creating it if absent.
// Vaihe 1 is single-tenant in data; this is the seam multi-tenancy slots into.
func ensureTenant(ctx context.Context, st *store.Store, name string) (string, error) {
	var id string
	err := st.Pool.QueryRow(ctx, `
		INSERT INTO tenant (name) VALUES ($1)
		ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
		RETURNING id::text`, name).Scan(&id)
	return id, err
}

// bootstrapAppInstall upserts a github_app_install row from env vars when
// FLOW_GITHUB_APP_ORG is set. Missing/blank ORG = no-op (the broker simply
// has no installations to serve). app_id and installation_id must parse as
// integers; private_key_ref is the *env var name* the EnvResolver looks up
// at mint-time, never the key itself.
func bootstrapAppInstall(ctx context.Context, st *store.Store, tenantID string) error {
	org := os.Getenv("FLOW_GITHUB_APP_ORG")
	if org == "" {
		return nil
	}
	appIDStr := os.Getenv("FLOW_GITHUB_APP_ID")
	installIDStr := os.Getenv("FLOW_GITHUB_APP_INSTALLATION_ID")
	keyRef := os.Getenv("FLOW_GITHUB_APP_PRIVATE_KEY_REF")
	if appIDStr == "" || installIDStr == "" || keyRef == "" {
		return errors.New("FLOW_GITHUB_APP_ORG is set but FLOW_GITHUB_APP_ID / FLOW_GITHUB_APP_INSTALLATION_ID / FLOW_GITHUB_APP_PRIVATE_KEY_REF is missing")
	}
	appID, err := strconv.ParseInt(appIDStr, 10, 64)
	if err != nil {
		return errors.New("FLOW_GITHUB_APP_ID must be an integer")
	}
	installID, err := strconv.ParseInt(installIDStr, 10, 64)
	if err != nil {
		return errors.New("FLOW_GITHUB_APP_INSTALLATION_ID must be an integer")
	}
	if _, err := st.Pool.Exec(ctx, `
		INSERT INTO github_app_install (tenant_id, org, app_id, installation_id, private_key_ref)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, org) DO UPDATE
		   SET app_id          = EXCLUDED.app_id,
		       installation_id = EXCLUDED.installation_id,
		       private_key_ref = EXCLUDED.private_key_ref`,
		tenantID, org, appID, installID, keyRef); err != nil {
		return err
	}
	log.Printf("flowd: bootstrap github-app install: tenant=%s org=%s app_id=%d install_id=%d key_ref=%s",
		tenantID, org, appID, installID, keyRef)
	return nil
}

func runReaper(ctx context.Context, m *lease.Manager) {
	ticker := time.NewTicker(lease.DefaultReapInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := m.Reap(ctx)
			if err != nil {
				log.Printf("reaper: %v", err)
				continue
			}
			if n > 0 {
				log.Printf("reaper: reaped %d expired lease(s)", n)
			}
		}
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func durationOr(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
