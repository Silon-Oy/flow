package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"testing"
)

// TestMe_AdminPayload locks the admin shape of /v1/me: the resolved principal's
// github_login + role flow through, and the capability list matches every §7
// row the admin holds. The dashboard branches off this list, so a regression
// here breaks the role-aware UI.
func TestMe_AdminPayload(t *testing.T) {
	ts, pool, tenantID, _ := newTestServer(t)
	login := "me-admin-" + tenantID
	adminToken := seedSession(t, pool, tenantID, "admin", login)

	resp := getWithToken(t, ts.URL+"/v1/me", adminToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", resp.StatusCode, readBody(t, resp))
	}
	var got struct {
		UserID       string   `json:"user_id"`
		GitHubLogin  string   `json:"github_login"`
		Role         string   `json:"role"`
		Capabilities []string `json:"capabilities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.UserID == "" {
		t.Errorf("user_id empty")
	}
	if got.GitHubLogin != login {
		t.Errorf("github_login = %q, want %q", got.GitHubLogin, login)
	}
	if got.Role != "admin" {
		t.Errorf("role = %q, want admin", got.Role)
	}

	want := []string{
		"github_app.manage",
		"merge_policy.manage",
		"project.register",
		"runner.register.self",
		"runners.manage.shared",
		"runs.view.own",
		"runs.view.tenant",
		"secrets.manage",
	}
	gotSorted := append([]string(nil), got.Capabilities...)
	sort.Strings(gotSorted)
	if !equalStrings(gotSorted, want) {
		t.Errorf("capabilities = %v, want %v", gotSorted, want)
	}
}

// TestMe_DeveloperPayload locks the developer shape: capabilities[] is the
// §7-row subset, and the admin-only rows (runners.manage.shared,
// secrets.manage, merge_policy.manage, github_app.manage, runs.view.tenant)
// are explicitly absent. This is the property the dashboard's panel-hiding
// logic relies on.
func TestMe_DeveloperPayload(t *testing.T) {
	ts, pool, tenantID, _ := newTestServer(t)
	login := "me-dev-" + tenantID
	devToken := seedSession(t, pool, tenantID, "developer", login)

	resp := getWithToken(t, ts.URL+"/v1/me", devToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", resp.StatusCode, readBody(t, resp))
	}
	var got struct {
		Role         string   `json:"role"`
		GitHubLogin  string   `json:"github_login"`
		Capabilities []string `json:"capabilities"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.GitHubLogin != login {
		t.Errorf("github_login = %q, want %q", got.GitHubLogin, login)
	}
	if got.Role != "developer" {
		t.Errorf("role = %q, want developer", got.Role)
	}

	want := []string{
		"project.register",
		"runner.register.self",
		"runs.view.own",
	}
	gotSorted := append([]string(nil), got.Capabilities...)
	sort.Strings(gotSorted)
	if !equalStrings(gotSorted, want) {
		t.Errorf("capabilities = %v, want %v", gotSorted, want)
	}

	// Belt-and-braces: an admin-only capability MUST NOT appear in the
	// developer's list (these are the rows the dashboard hides on).
	forbidden := []string{
		"runners.manage.shared",
		"secrets.manage",
		"merge_policy.manage",
		"github_app.manage",
		"runs.view.tenant",
	}
	for _, f := range forbidden {
		if contains(got.Capabilities, f) {
			t.Errorf("developer capabilities include admin-only %q: %v", f, got.Capabilities)
		}
	}
}

// TestMe_Unauthenticated proves the route is RequireAuth-gated: no bearer
// token = 401, not 200 with an empty payload.
func TestMe_Unauthenticated(t *testing.T) {
	ts, _, _, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/v1/me")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
