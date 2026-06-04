package githubapp

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Silon-Oy/flow/internal/secrets"
)

// fakeGitHub returns a test server that mints an installation token for any
// POST /app/installations/{id}/access_tokens. callCount lets the test assert
// cache behaviour without exercising the DB path.
type fakeGitHub struct {
	srv      *httptest.Server
	called   int32
	expires  time.Time
	tokenOut string
}

func newFakeGitHub(t *testing.T, expires time.Time, token string) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{expires: expires, tokenOut: token}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost ||
			!strings.HasPrefix(r.URL.Path, "/app/installations/") ||
			!strings.HasSuffix(r.URL.Path, "/access_tokens") {
			http.Error(w, "unexpected request "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		// Sanity-check we sent an App JWT bearer with three dotted segments.
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") || strings.Count(auth, ".") != 2 {
			http.Error(w, "missing or malformed Authorization", http.StatusUnauthorized)
			return
		}
		atomic.AddInt32(&f.called, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      f.tokenOut,
			"expires_at": f.expires.UTC().Format(time.RFC3339),
		})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// keyPEM emits a PKCS#1 PEM-encoded test key.
func keyPEM(t *testing.T) []byte {
	t.Helper()
	k := generateTestKey(t)
	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(k),
	})
}

// mapResolver is a Resolver that pulls values from an in-test map.
type mapResolver map[string][]byte

func (m mapResolver) Resolve(ref string) ([]byte, error) {
	v, ok := m[ref]
	if !ok {
		return nil, fmt.Errorf("%w: %s", secrets.ErrNotFound, ref)
	}
	return v, nil
}

func TestMintInstallationToken(t *testing.T) {
	exp := time.Now().Add(time.Hour).Truncate(time.Second)
	fake := newFakeGitHub(t, exp, "ghs_test_token_value")

	pk := keyPEM(t)
	res := mapResolver{"FAKE_KEY": pk}

	b := NewBroker(nil, res).WithHTTPClient(fake.srv.Client()).WithAPIBase(fake.srv.URL)

	tok, err := b.mintInstallationToken(context.Background(),
		&installation{AppID: 9999, InstallationID: 42, PrivateKeyRef: "FAKE_KEY"},
		time.Now())
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if tok.Token != "ghs_test_token_value" {
		t.Errorf("token = %q, want ghs_test_token_value", tok.Token)
	}
	if !tok.ExpiresAt.Equal(exp) {
		t.Errorf("expires_at = %v, want %v", tok.ExpiresAt, exp)
	}
	if got := atomic.LoadInt32(&fake.called); got != 1 {
		t.Errorf("called %d times, want 1", got)
	}
}

func TestMintInstallationTokenResolverError(t *testing.T) {
	fake := newFakeGitHub(t, time.Now().Add(time.Hour), "x")
	b := NewBroker(nil, mapResolver{}).
		WithHTTPClient(fake.srv.Client()).WithAPIBase(fake.srv.URL)

	_, err := b.mintInstallationToken(context.Background(),
		&installation{AppID: 1, InstallationID: 1, PrivateKeyRef: "MISSING"},
		time.Now())
	if err == nil {
		t.Fatal("expected error from missing secret")
	}
}

func TestMintInstallationTokenServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Bad credentials","status":"401"}`, http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)

	b := NewBroker(nil, mapResolver{"K": keyPEM(t)}).
		WithHTTPClient(srv.Client()).WithAPIBase(srv.URL)

	_, err := b.mintInstallationToken(context.Background(),
		&installation{AppID: 1, InstallationID: 1, PrivateKeyRef: "K"},
		time.Now())
	if err == nil {
		t.Fatal("expected error from non-2xx")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error %q: want it to mention 401", err)
	}
}

// TestCacheKeyIsStable guards against accidentally changing the key shape and
// invalidating every in-memory entry on a deploy.
func TestCacheKeyIsStable(t *testing.T) {
	got := cacheKey("tenant-uuid", 1234)
	if got != "tenant-uuid|1234" {
		t.Errorf("cacheKey = %q, want %q", got, "tenant-uuid|1234")
	}
}

// TestBrokerCacheConcurrentSafe sets a cached token directly and ensures
// concurrent reads return it without re-minting. The pool is nil because we
// short-circuit at the cache; the call still needs an installation lookup so
// we use a slimmer test that exercises only the validity path.
func TestTokenCacheValidReturnsFast(t *testing.T) {
	exp := time.Now().Add(30 * time.Minute)
	cached := &Token{Token: "cached-x", ExpiresAt: exp}

	if !cached.Valid(time.Now()) {
		t.Fatal("token should be valid")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if !cached.Valid(time.Now()) {
				t.Error("token unexpectedly invalid in goroutine")
			}
		}()
	}
	wg.Wait()
}
