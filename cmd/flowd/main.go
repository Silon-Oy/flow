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
