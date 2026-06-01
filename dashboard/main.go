// Command dashboard is a read-only web view of Flow state (decision 7: metadata
// + logs, NO agent prompts/diffs). It serves a single static page and reverse-
// proxies the read API + SSE log stream to flowd, so the browser talks to one
// origin. Kept deliberately lightweight.
package main

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"time"
)

func main() {
	addr := envOr("DASHBOARD_ADDR", ":8090")
	centralURL := envOr("FLOW_CENTRAL_URL", "http://localhost:8080")

	target, err := url.Parse(centralURL)
	if err != nil {
		log.Fatalf("dashboard: bad FLOW_CENTRAL_URL: %v", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	// SSE must not be buffered by the proxy, or the live tail stalls.
	proxy.FlushInterval = -1

	mux := http.NewServeMux()
	// Proxy the read API (runs, runners, egress) and SSE logs to flowd.
	mux.Handle("/v1/", proxy)
	// Static page.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(indexHTML))
	})

	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("dashboard listening on %s (central=%s)", addr, centralURL)
	log.Fatal(srv.ListenAndServe())
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
