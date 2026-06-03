package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silon-Oy/flow/internal/store"
)

// stubGitHub builds an httptest.Server that mimics the three GitHub endpoints
// the device-flow uses (login/device/code, login/oauth/access_token, api/user).
// `pendingPolls` lets a test exercise the authorization_pending branch before
// returning the access_token.
type stubOpts struct {
	clientID        string
	deviceCode      string
	userCode        string
	accessToken     string
	githubLogin     string
	pendingPolls    int
	forceAccessErr  string
	forceUserHTTP   int
}

func stubGitHub(t *testing.T, opts stubOpts) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	pendingLeft := opts.pendingPolls

	mux.HandleFunc("/login/device/code", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("device/code parse: %v", err)
		}
		if r.PostForm.Get("client_id") != opts.clientID {
			t.Errorf("device/code client_id = %q, want %q", r.PostForm.Get("client_id"), opts.clientID)
		}
		if r.PostForm.Get("scope") != "read:user" {
			t.Errorf("device/code scope = %q, want read:user", r.PostForm.Get("scope"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"device_code":      opts.deviceCode,
			"user_code":        opts.userCode,
			"verification_uri": "https://github.com/login/device",
			"expires_in":       900,
			"interval":         5,
		})
	})

	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("access_token parse: %v", err)
		}
		if r.PostForm.Get("device_code") != opts.deviceCode {
			t.Errorf("access_token device_code = %q, want %q", r.PostForm.Get("device_code"), opts.deviceCode)
		}
		if r.PostForm.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
			t.Errorf("access_token grant_type = %q", r.PostForm.Get("grant_type"))
		}
		w.Header().Set("Content-Type", "application/json")
		if opts.forceAccessErr != "" {
			_ = json.NewEncoder(w).Encode(map[string]string{"error": opts.forceAccessErr})
			return
		}
		if pendingLeft > 0 {
			pendingLeft--
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token": opts.accessToken,
			"token_type":   "bearer",
			"scope":        "read:user",
		})
	})

	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		gotAuth := r.Header.Get("Authorization")
		wantAuth := "Bearer " + opts.accessToken
		if gotAuth != wantAuth {
			t.Errorf("api/user Authorization = %q, want %q", gotAuth, wantAuth)
		}
		if opts.forceUserHTTP != 0 {
			w.WriteHeader(opts.forceUserHTTP)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"login": opts.githubLogin, "id": 42})
	})

	return httptest.NewServer(mux)
}

// newAuthHarness wires a Service against a throwaway-Postgres test DSN. Tests
// that don't need a real DB use newServiceNoDB to avoid the FLOW_TEST_DSN gate.
func newAuthHarness(t *testing.T, opts stubOpts) (*Service, *pgxpool.Pool, string) {
	t.Helper()
	dsn := os.Getenv("FLOW_TEST_DSN")
	if dsn == "" {
		t.Skip("FLOW_TEST_DSN not set — skipping auth DB integration test")
	}
	if err := store.Migrate(dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	// Per-test tenant so the unique app_user.(tenant_id, github_login) index
	// doesn't fight concurrent runs.
	tenantName := fmt.Sprintf("auth-%d", testNonce())
	var tenantID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO tenant (name) VALUES ($1) RETURNING id::text`, tenantName).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	gh := stubGitHub(t, opts)
	t.Cleanup(gh.Close)

	svc := New(pool, tenantID, opts.clientID)
	svc.GitHubBaseURL = gh.URL
	svc.GitHubAPIBaseURL = gh.URL
	return svc, pool, tenantID
}

func TestStartDeviceLogin_Success(t *testing.T) {
	gh := stubGitHub(t, stubOpts{
		clientID: "Iv1.test", deviceCode: "DEV-1", userCode: "ABCD-1234",
	})
	defer gh.Close()
	svc := &Service{
		ClientID:         "Iv1.test",
		GitHubBaseURL:    gh.URL,
		GitHubAPIBaseURL: gh.URL,
		HTTP:             gh.Client(),
	}
	got, err := svc.StartDeviceLogin(context.Background())
	if err != nil {
		t.Fatalf("StartDeviceLogin: %v", err)
	}
	if got.DeviceCode != "DEV-1" || got.UserCode != "ABCD-1234" {
		t.Errorf("device/user code mismatch: %+v", got)
	}
	if got.Interval != 5 {
		t.Errorf("interval = %d, want 5", got.Interval)
	}
}

func TestStartDeviceLogin_RequiresClientID(t *testing.T) {
	svc := &Service{}
	if _, err := svc.StartDeviceLogin(context.Background()); err == nil {
		t.Fatal("expected error when ClientID is empty")
	}
}

// TestPollDeviceLogin_PendingThenSuccess exercises the full flow against a real
// DB so the user upsert + session insert are verified end-to-end.
func TestPollDeviceLogin_PendingThenSuccess(t *testing.T) {
	svc, pool, tenantID := newAuthHarness(t, stubOpts{
		clientID:     "Iv1.test",
		deviceCode:   "DEV-OK",
		userCode:     "ZZZZ-9999",
		accessToken:  "gho_secrettoken",
		githubLogin:  "alice",
		pendingPolls: 2,
	})
	ctx := context.Background()

	// First two polls -> pending.
	for i := 0; i < 2; i++ {
		r, err := svc.PollDeviceLogin(ctx, "DEV-OK")
		if err != nil {
			t.Fatalf("poll #%d: %v", i, err)
		}
		if !r.Pending {
			t.Fatalf("poll #%d: expected pending, got %+v", i, r)
		}
	}

	// Third poll -> success.
	got, err := svc.PollDeviceLogin(ctx, "DEV-OK")
	if err != nil {
		t.Fatalf("poll success: %v", err)
	}
	if got.Pending {
		t.Fatal("expected non-pending on success")
	}
	if got.SessionToken == "" || got.GitHubLogin != "alice" {
		t.Errorf("unexpected success payload: %+v", got)
	}

	// User row exists with developer role.
	var role string
	if err := pool.QueryRow(ctx,
		`SELECT role::text FROM app_user WHERE tenant_id = $1 AND github_login = 'alice'`,
		tenantID).Scan(&role); err != nil {
		t.Fatalf("user lookup: %v", err)
	}
	if role != "developer" {
		t.Errorf("role = %q, want developer", role)
	}

	// user_session row exists and its hash matches the returned raw token.
	expectedHash := sha256.Sum256([]byte(got.SessionToken))
	var storedHash []byte
	if err := pool.QueryRow(ctx,
		`SELECT token_hash FROM user_session
		   WHERE user_id = (SELECT id FROM app_user WHERE tenant_id = $1 AND github_login = 'alice')`,
		tenantID).Scan(&storedHash); err != nil {
		t.Fatalf("session lookup: %v", err)
	}
	if hex.EncodeToString(storedHash) != hex.EncodeToString(expectedHash[:]) {
		t.Errorf("stored hash mismatch")
	}

	// The raw GitHub token must NOT appear anywhere persisted.
	var leaked int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM user_session
		 WHERE token_hash = decode($1, 'hex')`,
		hex.EncodeToString([]byte("gho_secrettoken"))).Scan(&leaked); err != nil {
		t.Fatalf("leak check: %v", err)
	}
	if leaked != 0 {
		t.Errorf("raw GitHub token leaked into user_session: %d rows", leaked)
	}
}

func TestPollDeviceLogin_AccessDenied(t *testing.T) {
	gh := stubGitHub(t, stubOpts{
		clientID: "Iv1.test", deviceCode: "DEV-X",
		forceAccessErr: "access_denied",
	})
	defer gh.Close()
	svc := &Service{
		ClientID: "Iv1.test", GitHubBaseURL: gh.URL, GitHubAPIBaseURL: gh.URL,
		HTTP: gh.Client(),
	}
	_, err := svc.PollDeviceLogin(context.Background(), "DEV-X")
	if err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("expected access denied, got %v", err)
	}
}

func TestPollDeviceLogin_RepeatedLoginReusesUser(t *testing.T) {
	svc, pool, tenantID := newAuthHarness(t, stubOpts{
		clientID:    "Iv1.test",
		deviceCode:  "DEV-RE",
		userCode:    "RE-1111",
		accessToken: "gho_again",
		githubLogin: "bob",
	})
	ctx := context.Background()
	if _, err := svc.PollDeviceLogin(ctx, "DEV-RE"); err != nil {
		t.Fatalf("first login: %v", err)
	}
	if _, err := svc.PollDeviceLogin(ctx, "DEV-RE"); err != nil {
		t.Fatalf("second login: %v", err)
	}
	var users, sessions int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM app_user WHERE tenant_id = $1 AND github_login = 'bob'`,
		tenantID).Scan(&users); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM user_session
		 WHERE user_id = (SELECT id FROM app_user WHERE tenant_id = $1 AND github_login = 'bob')`,
		tenantID).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if users != 1 {
		t.Errorf("users = %d, want 1 (upsert should not duplicate)", users)
	}
	if sessions != 2 {
		t.Errorf("sessions = %d, want 2 (each login mints a session)", sessions)
	}
}

func TestHashToken_Deterministic(t *testing.T) {
	a := HashToken("hello")
	b := HashToken("hello")
	if hex.EncodeToString(a) != hex.EncodeToString(b) {
		t.Fatal("HashToken not deterministic")
	}
	if hex.EncodeToString(HashToken("hello")) == hex.EncodeToString(HashToken("world")) {
		t.Fatal("HashToken collides on distinct inputs")
	}
}

// testNonce gives each test its own tenant name. Per-PID + sequence keeps
// parallel `go test` runs against the same DSN from colliding.
var testNonceCounter = 0

func testNonce() int {
	testNonceCounter++
	return os.Getpid()*1_000_000 + testNonceCounter
}
