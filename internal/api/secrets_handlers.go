package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Silon-Oy/flow/internal/secrets"
)

// secretCreateReq is the POST /v1/secrets wire shape (§9 admin path). The raw
// `value` is the plaintext; it is encrypted with the central's symmetric key
// (FLOW_SECRETS_DB_KEY) at INSERT time and never logged. `delivery` is the
// SECRET_REF.delivery enum: 'env' materialises at lease time into the
// container env, 'proxy' is accepted at schema level but the runtime path
// is not yet implemented (cycle review: out-of-scope for this issue).
type secretCreateReq struct {
	Key      string `json:"key"`
	Value    string `json:"value"`
	Delivery string `json:"delivery,omitempty"`
}

// secretCreateResp returns the new row's metadata. The plaintext is never
// echoed back — the caller already had it.
type secretCreateResp struct {
	ID       string `json:"id"`
	Key      string `json:"key"`
	Store    string `json:"store"`
	Delivery string `json:"delivery"`
}

// handleSecretCreate is the admin path for "write a secret". §7 row "Asettaa/
// muokkaa secretsejä" — admin-only via CapSecretsManage gate in Routes().
//
// The handler:
//  1. Refuses if the central has no symmetric key configured (503 — postgres
//     store is not available, EnvResolver-only deploys still work but cannot
//     accept a write here).
//  2. Validates key/value/delivery and rejects forbidden env keys
//     (GITHUB_TOKEN / GH_TOKEN must go through §11.3 proxy-injection).
//  3. Inserts secret_ref + secret_value in one tx, with the value encrypted
//     via pgp_sym_encrypt server-side so the plaintext never lands in the
//     application logs nor in the migrations.
//  4. Maps Postgres unique violation on (tenant_id, key) to HTTP 409.
//
// `delivery='proxy'` is currently accepted at the schema level (the row is
// stored) but the runtime materialiser returns ErrDeliveryNotSupported, so
// downstream callers see a clear "not yet supported" rather than a silent
// no-op.
func (s *Server) handleSecretCreate(w http.ResponseWriter, r *http.Request) {
	if len(s.SecretsKey) == 0 {
		writeErr(w, http.StatusServiceUnavailable, "secrets store not configured (FLOW_SECRETS_DB_KEY missing)")
		return
	}
	var req secretCreateReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body: "+err.Error())
		return
	}
	if req.Key == "" {
		writeErr(w, http.StatusBadRequest, "key required")
		return
	}
	if req.Value == "" {
		writeErr(w, http.StatusBadRequest, "value required")
		return
	}
	delivery := req.Delivery
	if delivery == "" {
		delivery = "env"
	}
	if delivery != "env" && delivery != "proxy" {
		writeErr(w, http.StatusBadRequest, "delivery must be env or proxy")
		return
	}
	// §11.3 defense-in-depth: never let GH credentials ride the env path even
	// if an admin asks. The proxy-injection broker is the only sanctioned
	// channel for these.
	if delivery == "env" && secrets.IsForbiddenEnvKey(req.Key) {
		writeErr(w, http.StatusBadRequest, "key "+req.Key+" must use delivery=proxy (§11.3)")
		return
	}

	tenantID := tenantFromCtx(r.Context())
	ctx, cancel := withTimeout(r, 5*time.Second)
	defer cancel()

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, err.Error())
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var refID string
	err = tx.QueryRow(ctx, `
		INSERT INTO secret_ref (tenant_id, key, store, path, delivery)
		VALUES ($1, $2, 'postgres', '', $3)
		RETURNING id::text`,
		tenantID, req.Key, delivery,
	).Scan(&refID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			writeErr(w, http.StatusConflict, "secret with this key already exists")
			return
		}
		writeErr(w, http.StatusInternalServerError, "insert secret_ref: "+err.Error())
		return
	}

	// Encryption happens DB-side: pgp_sym_encrypt(plaintext, key). The key
	// rides the libpq connection but never lands in the application log or
	// the SQL migration history (parameters are not logged by default).
	if _, err := tx.Exec(ctx, `
		INSERT INTO secret_value (ref_id, ciphertext)
		VALUES ($1::uuid, pgp_sym_encrypt($2, $3))`,
		refID, req.Value, string(s.SecretsKey),
	); err != nil {
		writeErr(w, http.StatusInternalServerError, "insert secret_value: "+err.Error())
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeErr(w, http.StatusInternalServerError, "commit: "+err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, secretCreateResp{
		ID:       refID,
		Key:      req.Key,
		Store:    "postgres",
		Delivery: delivery,
	})
}
