package secrets

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Silon-Oy/flow/internal/store"
)

func TestEnvResolver(t *testing.T) {
	r := EnvResolver{}

	t.Run("empty ref", func(t *testing.T) {
		_, err := r.Resolve("")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("got %v, want ErrNotFound", err)
		}
	})

	t.Run("unset env", func(t *testing.T) {
		t.Setenv("FLOW_TEST_SECRET_UNSET", "")
		// t.Setenv sets to empty — emulate fully-unset by Unsetenv via os.
		// We rely on the variable being empty: LookupEnv returns ok=true but
		// value=="", which the resolver treats as not-found.
		_, err := r.Resolve("FLOW_TEST_SECRET_UNSET")
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("empty value: got %v, want ErrNotFound", err)
		}
	})

	t.Run("present", func(t *testing.T) {
		t.Setenv("FLOW_TEST_SECRET_PRESENT", "hunter2")
		got, err := r.Resolve("FLOW_TEST_SECRET_PRESENT")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if string(got) != "hunter2" {
			t.Fatalf("got %q, want %q", got, "hunter2")
		}
	})
}

// stubResolver is a deterministic Resolver for unit tests. It records the last
// ref it saw so routing-correctness assertions can pin "was it called with the
// uuid or the path?". Pointer receiver so mutations persist across calls.
type stubResolver struct {
	value   string
	lastRef string
}

func (s *stubResolver) Resolve(ref string) ([]byte, error) {
	s.lastRef = ref
	return []byte(s.value), nil
}

// TestRegistry_Routing covers the {store -> resolver} dispatch table without a
// DB: stubs stand in for both EnvResolver and PGCryptoResolver so the routing
// logic is testable in unit mode.
func TestRegistry_Routing(t *testing.T) {
	envStub := &stubResolver{value: "env-value"}
	pgStub := &stubResolver{value: "pg-value"}
	reg := &Registry{Env: envStub, Pg: pgStub}

	cases := []struct {
		name    string
		store   string
		ref     string
		want    string
		wantErr bool
	}{
		{name: "env store", store: "env", ref: "X", want: "env-value"},
		{name: "empty store defaults to env", store: "", ref: "X", want: "env-value"},
		{name: "postgres store", store: "postgres", ref: "uuid-1", want: "pg-value"},
		{name: "unknown store fails closed", store: "vault", ref: "X", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := reg.Resolve(tc.store, tc.ref)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got value %q", got)
				}
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("got %v, want ErrNotFound", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRegistry_NilResolverFailsClosed: a half-configured deploy returns
// ErrNotFound rather than panicking.
func TestRegistry_NilResolverFailsClosed(t *testing.T) {
	reg := &Registry{Env: nil, Pg: nil}
	if _, err := reg.Resolve("env", "X"); !errors.Is(err, ErrNotFound) {
		t.Errorf("nil env resolver: got %v, want ErrNotFound", err)
	}
	if _, err := reg.Resolve("postgres", "X"); !errors.Is(err, ErrNotFound) {
		t.Errorf("nil pg resolver: got %v, want ErrNotFound", err)
	}
}

// TestMaterializeEnv_HappyPath_EnvStore: store=env routes to EnvResolver with
// path as the env var name.
func TestMaterializeEnv_HappyPath_EnvStore(t *testing.T) {
	t.Setenv("FLOW_TEST_MAT_ENV", "shipit")
	reg := &Registry{Env: EnvResolver{}}
	ref := SecretRef{
		ID:       "ignored-for-env-store",
		Key:      "MY_VAR",
		Store:    "env",
		Path:     "FLOW_TEST_MAT_ENV",
		Delivery: "env",
	}
	k, v, err := MaterializeEnv(reg, ref)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if k != "MY_VAR" || v != "shipit" {
		t.Fatalf("got (%q, %q), want (MY_VAR, shipit)", k, v)
	}
}

// TestMaterializeEnv_HappyPath_PostgresStore: store=postgres routes to the
// PGCryptoResolver stub with ID (not Path) as the lookup ref. Locks the
// store-aware ref selection.
func TestMaterializeEnv_HappyPath_PostgresStore(t *testing.T) {
	stub := &stubResolver{value: "secret-payload"}
	reg := &Registry{Pg: stub}
	ref := SecretRef{
		ID:       "00000000-0000-0000-0000-000000000001",
		Key:      "DB_URL",
		Store:    "postgres",
		Path:     "ignored-for-postgres-store",
		Delivery: "env",
	}
	k, v, err := MaterializeEnv(reg, ref)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if k != "DB_URL" || v != "secret-payload" {
		t.Fatalf("got (%q, %q), want (DB_URL, secret-payload)", k, v)
	}
	// The PGCryptoResolver was queried with the uuid, NOT the path.
	if stub.lastRef != "00000000-0000-0000-0000-000000000001" {
		t.Errorf("pg resolver called with %q, want the uuid", stub.lastRef)
	}
}

// TestMaterializeEnv_ProxyDelivery: delivery='proxy' is schema-valid but
// runtime refuses (the cycle review's "not yet supported"). Locks §11.3:
// proxy-injection is a separate issue and must not silently no-op as env.
func TestMaterializeEnv_ProxyDelivery(t *testing.T) {
	reg := &Registry{Env: EnvResolver{}}
	ref := SecretRef{
		Key:      "PROXY_CRED",
		Store:    "postgres",
		Delivery: "proxy",
	}
	_, _, err := MaterializeEnv(reg, ref)
	if !errors.Is(err, ErrDeliveryNotSupported) {
		t.Fatalf("got %v, want ErrDeliveryNotSupported", err)
	}
}

// TestMaterializeEnv_ForbiddenKeys: GITHUB_TOKEN / GH_TOKEN never materialize
// as env, regardless of how the row was inserted. The defense-in-depth pair
// to IsForbiddenEnvKey at the API layer.
func TestMaterializeEnv_ForbiddenKeys(t *testing.T) {
	reg := &Registry{Env: &stubResolver{value: "should-not-be-returned"}}
	for _, k := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		ref := SecretRef{
			Key:      k,
			Store:    "env",
			Path:     "WHATEVER",
			Delivery: "env",
		}
		if _, _, err := MaterializeEnv(reg, ref); !errors.Is(err, ErrDeliveryNotSupported) {
			t.Errorf("%s: got %v, want ErrDeliveryNotSupported", k, err)
		}
	}
}

// TestPGCryptoResolver_Roundtrip exercises the real DB path: encrypt with
// pgp_sym_encrypt, decrypt via the resolver, and assert the plaintext round-
// trips. Skipped when FLOW_TEST_DSN is unset (same convention as the rest of
// the DB-bound tests).
func TestPGCryptoResolver_Roundtrip(t *testing.T) {
	pool, cleanup := openTestPool(t)
	defer cleanup()

	ctx := context.Background()
	tenantName := fmt.Sprintf("secrets-test-%d", time.Now().UnixNano())
	var tenantID string
	if err := pool.QueryRow(ctx, `INSERT INTO tenant (name) VALUES ($1) RETURNING id::text`, tenantName).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tenant WHERE id = $1`, tenantID)
	})

	const key = "test-symmetric-key-do-not-ship"
	const plaintext = "postgres://flow:flow@db/test"

	var refID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO secret_ref (tenant_id, key, store, path, delivery)
		VALUES ($1, 'DB_URL_TEST', 'postgres', '', 'env')
		RETURNING id::text`,
		tenantID).Scan(&refID); err != nil {
		t.Fatalf("insert secret_ref: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO secret_value (ref_id, ciphertext)
		VALUES ($1::uuid, pgp_sym_encrypt($2, $3))`,
		refID, plaintext, key); err != nil {
		t.Fatalf("insert secret_value: %v", err)
	}

	res := &PGCryptoResolver{Pool: pool, Key: []byte(key)}
	got, err := res.Resolve(refID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if string(got) != plaintext {
		t.Fatalf("roundtrip mismatch: got %q, want %q", got, plaintext)
	}

	// Wrong key surfaces an error rather than silent garbage.
	bad := &PGCryptoResolver{Pool: pool, Key: []byte("wrong-key")}
	if _, err := bad.Resolve(refID); err == nil {
		t.Errorf("wrong key: expected error, got nil")
	}

	// Unknown ref id is ErrNotFound.
	if _, err := res.Resolve("00000000-0000-0000-0000-000000000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown ref: got %v, want ErrNotFound", err)
	}
}

// TestMaterializeAllEnvForTenant_DB walks the full tenant -> env-map path:
// seed two delivery=env secret_refs (one env-store, one postgres-store) and
// one delivery=proxy row, verify the materialized map carries exactly the env
// ones with the expected plaintext.
func TestMaterializeAllEnvForTenant_DB(t *testing.T) {
	pool, cleanup := openTestPool(t)
	defer cleanup()

	ctx := context.Background()
	tenantName := fmt.Sprintf("mat-test-%d", time.Now().UnixNano())
	var tenantID string
	if err := pool.QueryRow(ctx, `INSERT INTO tenant (name) VALUES ($1) RETURNING id::text`, tenantName).Scan(&tenantID); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM tenant WHERE id = $1`, tenantID)
	})

	const key = "mat-key"
	t.Setenv("FLOW_MAT_TEST_ENV", "from-env")

	// env-store, delivery=env: path is the env var name.
	if _, err := pool.Exec(ctx, `
		INSERT INTO secret_ref (tenant_id, key, store, path, delivery)
		VALUES ($1, 'FROM_ENV', 'env', 'FLOW_MAT_TEST_ENV', 'env')`, tenantID); err != nil {
		t.Fatalf("seed env ref: %v", err)
	}

	// postgres-store, delivery=env: secret_value carries the encrypted value.
	var pgRefID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO secret_ref (tenant_id, key, store, path, delivery)
		VALUES ($1, 'FROM_PG', 'postgres', '', 'env')
		RETURNING id::text`, tenantID).Scan(&pgRefID); err != nil {
		t.Fatalf("seed pg ref: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO secret_value (ref_id, ciphertext)
		VALUES ($1::uuid, pgp_sym_encrypt('from-pg', $2))`, pgRefID, key); err != nil {
		t.Fatalf("seed pg value: %v", err)
	}

	// delivery=proxy: should be skipped by MaterializeAllEnvForTenant (the
	// SELECT filters delivery='env'), so its presence does NOT poison the map.
	if _, err := pool.Exec(ctx, `
		INSERT INTO secret_ref (tenant_id, key, store, path, delivery)
		VALUES ($1, 'PROXY_ONLY', 'postgres', '', 'proxy')`, tenantID); err != nil {
		t.Fatalf("seed proxy ref: %v", err)
	}

	reg := &Registry{
		Env: EnvResolver{},
		Pg:  &PGCryptoResolver{Pool: pool, Key: []byte(key)},
	}
	got, err := MaterializeAllEnvForTenant(ctx, pool, reg, tenantID)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if got["FROM_ENV"] != "from-env" {
		t.Errorf("FROM_ENV = %q, want %q", got["FROM_ENV"], "from-env")
	}
	if got["FROM_PG"] != "from-pg" {
		t.Errorf("FROM_PG = %q, want %q", got["FROM_PG"], "from-pg")
	}
	if _, ok := got["PROXY_ONLY"]; ok {
		t.Errorf("PROXY_ONLY leaked into env map (delivery=proxy must NOT materialize as env)")
	}
	if len(got) != 2 {
		t.Errorf("got %d entries, want 2: %v", len(got), got)
	}
}

// openTestPool centralises the FLOW_TEST_DSN skip + pool open used by the
// DB-bound tests in this package. The DSN must already point at a migrated
// database (the api/store packages own the migrate helper).
func openTestPool(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()
	dsn := os.Getenv("FLOW_TEST_DSN")
	if dsn == "" {
		t.Skip("FLOW_TEST_DSN not set — skipping pgcrypto DB test")
	}
	if err := store.Migrate(dsn); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	return pool, pool.Close
}
