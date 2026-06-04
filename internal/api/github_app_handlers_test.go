package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silon-Oy/flow/internal/githubapp"
	"github.com/Silon-Oy/flow/internal/secrets"
	"github.com/Silon-Oy/flow/internal/store"
)

func seedAppInstall(t *testing.T, pool *pgxpool.Pool, tenantID, org, keyRef string, appID, installationID int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO github_app_install (tenant_id, org, app_id, installation_id, private_key_ref)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, org) DO UPDATE
		   SET app_id = EXCLUDED.app_id,
		       installation_id = EXCLUDED.installation_id,
		       private_key_ref = EXCLUDED.private_key_ref`,
		tenantID, org, appID, installationID, keyRef); err != nil {
		t.Fatalf("seed app install: %v", err)
	}
}

// pemKey generates an in-memory PKCS#1 PEM-encoded RSA private key. We mint
// one per test rather than hardcoding a fixture: the test never hits real
// GitHub and our fake server doesn't verify the signature.
func pemKey(t *testing.T) string {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(k),
	}))
}

func TestGitHubAppTokenIntegration(t *testing.T) {
	dsn := os.Getenv("FLOW_TEST_DSN")
	if dsn == "" {
		t.Skip("FLOW_TEST_DSN not set — skipping github-app integration test")
	}
	if err := store.Migrate(dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	tenantName := "ghapp-" + strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	var tenantID string
	if err := pool.QueryRow(ctx, `INSERT INTO tenant (name) VALUES ($1) RETURNING id::text`, tenantName).Scan(&tenantID); err != nil {
		t.Fatalf("tenant: %v", err)
	}

	// Fake GitHub for the broker to mint against.
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/app/installations/") || !strings.HasSuffix(r.URL.Path, "/access_tokens") {
			http.Error(w, "no", http.StatusNotFound)
			return
		}
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			http.Error(w, "no bearer", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_integration_test",
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	}))
	t.Cleanup(fake.Close)

	keyRef := "FLOW_TEST_APP_PRIVATE_KEY_" + strings.ToUpper(strings.ReplaceAll(tenantName, "-", "_"))
	t.Setenv(keyRef, pemKey(t))
	seedAppInstall(t, pool, tenantID, "acme", keyRef, 1001, 2002)

	const brokerToken = "test-broker-token-abc123"

	// authedGet issues a GET with the broker bearer attached. A plain
	// http.Get hits the unauthenticated path (covered separately).
	authedGet := func(url string) *http.Response {
		req, _ := http.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("Authorization", "Bearer "+brokerToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	srv := New(pool, tenantID)
	srv.BrokerToken = brokerToken
	srv.GHApp = githubapp.NewBroker(pool, secrets.EnvResolver{}).
		WithHTTPClient(fake.Client()).WithAPIBase(fake.URL)
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)

	t.Run("OK with bearer", func(t *testing.T) {
		resp := authedGet(ts.URL + "/v1/github-app/token?org=acme")
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d: %s", resp.StatusCode, body)
		}
		var out struct {
			Token     string    `json:"token"`
			ExpiresAt time.Time `json:"expires_at"`
		}
		if err := json.Unmarshal(body, &out); err != nil {
			t.Fatal(err)
		}
		if out.Token != "ghs_integration_test" {
			t.Errorf("token = %q", out.Token)
		}
		if out.ExpiresAt.IsZero() {
			t.Error("expires_at zero")
		}
	})

	t.Run("no bearer → 401", func(t *testing.T) {
		resp, err := http.Get(ts.URL + "/v1/github-app/token?org=acme")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status %d, want 401", resp.StatusCode)
		}
	})

	t.Run("wrong bearer → 401", func(t *testing.T) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/github-app/token?org=acme", nil)
		req.Header.Set("Authorization", "Bearer wrong-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("status %d, want 401", resp.StatusCode)
		}
	})

	t.Run("missing org → 400", func(t *testing.T) {
		resp := authedGet(ts.URL + "/v1/github-app/token")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status %d, want 400", resp.StatusCode)
		}
	})

	t.Run("unknown org → 404", func(t *testing.T) {
		resp := authedGet(ts.URL + "/v1/github-app/token?org=nope")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status %d, want 404", resp.StatusCode)
		}
	})

	t.Run("broker disabled → 503", func(t *testing.T) {
		// Spin up a fresh server with no BrokerToken set: even with bearer,
		// the central refuses to mint (fail-closed default).
		bare := New(pool, tenantID)
		bare.BrokerToken = "" // explicit: ignore whatever the env held
		bare.GHApp = srv.GHApp
		bareTS := httptest.NewServer(bare.Routes())
		t.Cleanup(bareTS.Close)

		resp := authedGet(bareTS.URL + "/v1/github-app/token?org=acme")
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Errorf("status %d, want 503", resp.StatusCode)
		}
	})
}
