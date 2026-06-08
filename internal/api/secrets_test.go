package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// TestSecretsCreate_AdminCreatesPostgresStore_RoundTrips walks the full happy
// path: an admin POST /v1/secrets stores a delivery=env secret, the row turns
// up in secret_ref + secret_value, and the ciphertext decrypts to the
// plaintext we posted. The handler is the only place that takes raw values,
// so the roundtrip is the wire-level contract.
func TestSecretsCreate_AdminCreatesPostgresStore_RoundTrips(t *testing.T) {
	t.Setenv("FLOW_SECRETS_DB_KEY", "test-key-for-secrets")
	ts, pool, tenantID, _ := newTestServer(t)
	adminToken := seedAdminSession(t, pool, tenantID)
	ctx := context.Background()

	resp, body := postAuth(t, ts.URL+"/v1/secrets", adminToken, map[string]any{
		"key":      "DATABASE_URL_TEST",
		"value":    "postgres://flow:flow@db/test",
		"delivery": "env",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", resp.StatusCode, body)
	}
	var out struct {
		ID       string `json:"id"`
		Key      string `json:"key"`
		Store    string `json:"store"`
		Delivery string `json:"delivery"`
	}
	mustJSON(t, body, &out)
	if out.ID == "" {
		t.Fatal("empty id in response")
	}
	if out.Key != "DATABASE_URL_TEST" || out.Store != "postgres" || out.Delivery != "env" {
		t.Errorf("got %+v, want key=DATABASE_URL_TEST store=postgres delivery=env", out)
	}

	// Ciphertext is real: decrypt with the same key and compare to plaintext.
	var got string
	if err := pool.QueryRow(ctx, `
		SELECT pgp_sym_decrypt(sv.ciphertext, $1)
		  FROM secret_value sv
		 WHERE sv.ref_id = $2::uuid`,
		"test-key-for-secrets", out.ID,
	).Scan(&got); err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got != "postgres://flow:flow@db/test" {
		t.Errorf("roundtrip mismatch: got %q", got)
	}
}

// TestSecretsCreate_DefaultsToEnv: empty delivery defaults to env, matching
// the secret_ref.delivery column default and avoiding "forgot to set it"
// foot-guns at the wire.
func TestSecretsCreate_DefaultsToEnv(t *testing.T) {
	t.Setenv("FLOW_SECRETS_DB_KEY", "test-key-default")
	ts, pool, tenantID, _ := newTestServer(t)
	adminToken := seedAdminSession(t, pool, tenantID)

	resp, body := postAuth(t, ts.URL+"/v1/secrets", adminToken, map[string]any{
		"key":   "MY_VAR",
		"value": "hello",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d (body=%s)", resp.StatusCode, body)
	}
	var out struct{ Delivery string }
	mustJSON(t, body, &out)
	if out.Delivery != "env" {
		t.Errorf("delivery = %q, want env (default)", out.Delivery)
	}
}

// TestSecretsCreate_RejectsForbiddenEnvKeys locks the §11.3 invariant at the
// API surface: GITHUB_TOKEN / GH_TOKEN cannot ride the env path. The cycle
// review left "delivery=proxy" runtime to a follow-up, so an env attempt is a
// 400.
func TestSecretsCreate_RejectsForbiddenEnvKeys(t *testing.T) {
	t.Setenv("FLOW_SECRETS_DB_KEY", "test-key-forbidden")
	ts, pool, tenantID, _ := newTestServer(t)
	adminToken := seedAdminSession(t, pool, tenantID)

	for _, key := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		resp, body := postAuth(t, ts.URL+"/v1/secrets", adminToken, map[string]any{
			"key":   key,
			"value": "ghp_secret_value",
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (body=%s)", key, resp.StatusCode, body)
		}
		if !strings.Contains(string(body), "§11.3") {
			t.Errorf("%s: response %q missing §11.3 reference", key, body)
		}
	}
}

// TestSecretsCreate_DeveloperForbidden locks the §7 RBAC row "Asettaa/muokkaa
// secretsejä": developer → 403, admin → 201. The CapSecretsManage capability
// is the single gate keeping a developer from writing tenant-wide secrets.
func TestSecretsCreate_DeveloperForbidden(t *testing.T) {
	t.Setenv("FLOW_SECRETS_DB_KEY", "test-key-rbac")
	ts, pool, tenantID, _ := newTestServer(t)
	devToken := seedSession(t, pool, tenantID, "developer", "dev-secrets-"+tenantID)

	resp, body := postAuth(t, ts.URL+"/v1/secrets", devToken, map[string]any{
		"key":   "MY_VAR",
		"value": "v",
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (body=%s)", resp.StatusCode, body)
	}
}

// TestSecretsCreate_Unauthenticated: no bearer → 401, never 400. The auth
// chain runs BEFORE the body validator so probing the wire is cheap and the
// validator never leaks the auth posture.
func TestSecretsCreate_Unauthenticated(t *testing.T) {
	t.Setenv("FLOW_SECRETS_DB_KEY", "test-key-unauth")
	ts, _, _, _ := newTestServer(t)

	resp, body := post(t, ts.URL+"/v1/secrets", map[string]any{
		"key":   "MY_VAR",
		"value": "v",
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (body=%s)", resp.StatusCode, body)
	}
}

// TestSecretsCreate_DuplicateKeyConflict: per-tenant (tenant_id, key) is
// UNIQUE in the schema; the handler maps that to 409 instead of leaking the
// raw pg error.
func TestSecretsCreate_DuplicateKeyConflict(t *testing.T) {
	t.Setenv("FLOW_SECRETS_DB_KEY", "test-key-dup")
	ts, pool, tenantID, _ := newTestServer(t)
	adminToken := seedAdminSession(t, pool, tenantID)

	body := map[string]any{"key": "DUP_KEY", "value": "v"}
	if resp, b := postAuth(t, ts.URL+"/v1/secrets", adminToken, body); resp.StatusCode != http.StatusCreated {
		t.Fatalf("first create: %d (body=%s)", resp.StatusCode, b)
	}
	resp, b := postAuth(t, ts.URL+"/v1/secrets", adminToken, body)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("second create: status = %d, want 409 (body=%s)", resp.StatusCode, b)
	}
}

// TestSecretsCreate_NoKeyConfigured: with FLOW_SECRETS_DB_KEY unset the
// handler must refuse rather than silently store an unencrypted (or empty-
// keyed) row. 503 keeps "store not configured" distinguishable from "you sent
// a bad body".
func TestSecretsCreate_NoKeyConfigured(t *testing.T) {
	// Override to empty BEFORE building the server so New() picks it up.
	t.Setenv("FLOW_SECRETS_DB_KEY", "")
	ts, pool, tenantID, _ := newTestServer(t)
	adminToken := seedAdminSession(t, pool, tenantID)

	resp, body := postAuth(t, ts.URL+"/v1/secrets", adminToken, map[string]any{
		"key":   "MY_VAR",
		"value": "v",
	})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (body=%s)", resp.StatusCode, body)
	}
}

// TestLeaseAcquire_ReturnsMaterialisedEnv proves the §9 wire contract: an
// admin-set delivery='env' secret turns up in the POST /v1/leases/acquire
// response under `env` so the runner can hand it to runnerexec.Spec.Env
// without ever reading secret_value itself. Locks the round-trip from
// POST /v1/secrets → secret_ref → MaterializeAllEnvForTenant → wire.
func TestLeaseAcquire_ReturnsMaterialisedEnv(t *testing.T) {
	t.Setenv("FLOW_SECRETS_DB_KEY", "lease-env-key")
	ts, pool, tenantID, projectID := newTestServer(t)
	ctx := context.Background()

	// 1. Admin stores a secret via the real handler.
	adminToken := seedAdminSession(t, pool, tenantID)
	resp, body := postAuth(t, ts.URL+"/v1/secrets", adminToken, map[string]any{
		"key":      "DB_URL_TEST",
		"value":    "postgres://flow:flow@db/test",
		"delivery": "env",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create secret: %d (body=%s)", resp.StatusCode, body)
	}

	// 2. Register a runner + seed work so Acquire returns a row.
	regResp, regBody := post(t, ts.URL+"/v1/runners/register", map[string]any{"hostname": "h", "capacity": 1})
	if regResp.StatusCode != http.StatusCreated {
		t.Fatalf("register: %d", regResp.StatusCode)
	}
	var reg struct {
		RunnerID    string `json:"runner_id"`
		RunnerToken string `json:"runner_token"`
	}
	mustJSON(t, regBody, &reg)

	wk := "leaseenv-wk-" + tenantID
	if _, err := pool.Exec(ctx,
		`INSERT INTO claimable_work (tenant_id, project_id, work_key, remote, issue_number, kind)
		 VALUES ($1,$2,$3,'origin',77,'develop')`, tenantID, projectID, wk); err != nil {
		t.Fatal(err)
	}

	// 3. Acquire and verify the env map contains our secret.
	acqResp, acqBody := postAuth(t, ts.URL+"/v1/leases/acquire", reg.RunnerToken,
		map[string]any{"runner_id": reg.RunnerID, "kinds": []string{"develop"}})
	if acqResp.StatusCode != http.StatusOK {
		t.Fatalf("acquire: %d (body=%s)", acqResp.StatusCode, acqBody)
	}
	var acq struct {
		Env map[string]string `json:"env"`
	}
	mustJSON(t, acqBody, &acq)
	if got := acq.Env["DB_URL_TEST"]; got != "postgres://flow:flow@db/test" {
		t.Errorf("DB_URL_TEST = %q, want %q (full env=%v)", got, "postgres://flow:flow@db/test", acq.Env)
	}
}

// TestLeaseAcquire_ProxyDeliveryDoesNotPoisonEnv: a delivery='proxy' row in
// the same tenant must NOT make MaterializeAllEnvForTenant fail at lease
// time — it is filtered at the SELECT. The wire stays usable while the
// proxy-injection issue is unimplemented.
func TestLeaseAcquire_ProxyDeliveryDoesNotPoisonEnv(t *testing.T) {
	t.Setenv("FLOW_SECRETS_DB_KEY", "lease-proxy-key")
	ts, pool, tenantID, projectID := newTestServer(t)
	ctx := context.Background()

	// Seed a delivery='proxy' row directly: POST /v1/secrets accepts both,
	// but using SQL keeps this test independent of the create wire shape.
	if _, err := pool.Exec(ctx, `
		INSERT INTO secret_ref (tenant_id, key, store, path, delivery)
		VALUES ($1, 'PROXY_CRED', 'postgres', '', 'proxy')`, tenantID); err != nil {
		t.Fatal(err)
	}

	// Acquire path: register runner, seed work, acquire.
	regResp, regBody := post(t, ts.URL+"/v1/runners/register", map[string]any{"hostname": "h", "capacity": 1})
	if regResp.StatusCode != http.StatusCreated {
		t.Fatalf("register: %d", regResp.StatusCode)
	}
	var reg struct {
		RunnerID    string `json:"runner_id"`
		RunnerToken string `json:"runner_token"`
	}
	mustJSON(t, regBody, &reg)

	wk := "proxyenv-wk-" + tenantID
	if _, err := pool.Exec(ctx,
		`INSERT INTO claimable_work (tenant_id, project_id, work_key, remote, issue_number, kind)
		 VALUES ($1,$2,$3,'origin',88,'develop')`, tenantID, projectID, wk); err != nil {
		t.Fatal(err)
	}

	acqResp, acqBody := postAuth(t, ts.URL+"/v1/leases/acquire", reg.RunnerToken,
		map[string]any{"runner_id": reg.RunnerID, "kinds": []string{"develop"}})
	if acqResp.StatusCode != http.StatusOK {
		t.Fatalf("acquire: %d (body=%s)", acqResp.StatusCode, acqBody)
	}
	var acq struct {
		Env map[string]string `json:"env"`
	}
	mustJSON(t, acqBody, &acq)
	if _, ok := acq.Env["PROXY_CRED"]; ok {
		t.Errorf("delivery=proxy leaked into env map: %v", acq.Env)
	}
}

