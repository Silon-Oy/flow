package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTenantFromContext_EmptyByDefault(t *testing.T) {
	if v, ok := TenantFromContext(context.Background()); ok || v != "" {
		t.Errorf("bare context = (%q,%v), want (\"\",false)", v, ok)
	}
}

func TestWithTenantContext_RoundTrip(t *testing.T) {
	ctx := WithTenantContext(context.Background(), "tenant-A")
	v, ok := TenantFromContext(ctx)
	if !ok || v != "tenant-A" {
		t.Errorf("got (%q,%v), want (\"tenant-A\",true)", v, ok)
	}
}

func TestWithTenant_PinsHeaderTenant(t *testing.T) {
	var seen string
	h := WithTenant(HeaderExtractor("bootstrap"))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, _ := TenantFromContext(r.Context())
		seen = v
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("X-Flow-Tenant-ID", "tenant-B")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if seen != "tenant-B" {
		t.Errorf("pinned tenant = %q, want tenant-B", seen)
	}
}

func TestWithTenant_FallsBackToBootstrap(t *testing.T) {
	var seen string
	h := WithTenant(HeaderExtractor("tenant-boot"))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v, _ := TenantFromContext(r.Context())
		seen = v
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if seen != "tenant-boot" {
		t.Errorf("fallback tenant = %q, want tenant-boot", seen)
	}
}

func TestWithTenant_RejectsWhenExtractorReturnsEmpty(t *testing.T) {
	reached := false
	h := WithTenant(func(*http.Request) (string, error) { return "", nil })(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if reached {
		t.Errorf("handler should not run when tenant unresolved")
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Errorf("body not JSON: %s", rec.Body.String())
	}
	if !strings.Contains(body["error"], "tenant") {
		t.Errorf("error body = %q, want tenant message", rec.Body.String())
	}
}

func TestWithTenant_RejectsWhenExtractorErrors(t *testing.T) {
	reached := false
	h := WithTenant(func(*http.Request) (string, error) { return "", errors.New("bad token") })(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
	if reached {
		t.Errorf("handler should not run when extractor errors")
	}
}
