// Package githubapp is the central's GitHub App token broker (§7.3): one App
// installation per (tenant, org) row in github_app_install, JWT→installation
// token mint via GitHub's API, in-process cache so a runner barrage doesn't
// re-hit GitHub on every lease.
//
// The package is structured so the verb-heavy bits (RS256 JWT assembly,
// installation_id lookup, cache validity) are pure functions testable without
// the network — only Broker.Token wires them into the HTTP round-trip.
package githubapp

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"time"
)

// AppJWTTTL is how long the App JWT we mint is valid. GitHub caps it at 10
// minutes; we use 9 to leave a clock-skew buffer (matches `github-app-auth.sh`
// behaviour).
const AppJWTTTL = 9 * time.Minute

// jwtSkew biases `iat` into the past so a slightly-fast central clock does not
// produce a token GitHub considers "issued in the future".
const jwtSkew = 60 * time.Second

// ParseRSAPrivateKey accepts a PEM-encoded RSA private key (PKCS#1 or PKCS#8)
// and returns the parsed *rsa.PrivateKey. Both formats are accepted because
// GitHub Apps download keys as PKCS#1 ("RSA PRIVATE KEY") but ops tooling
// (openssl, jwt-cli, kubernetes-secrets) often re-encodes them as PKCS#8
// ("PRIVATE KEY").
func ParseRSAPrivateKey(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("githubapp: no PEM block found")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("githubapp: PKCS#8 key is %T, not RSA", k)
		}
		return rk, nil
	default:
		return nil, fmt.Errorf("githubapp: unsupported PEM type %q", block.Type)
	}
}

// NewAppJWT builds and signs an RS256 JWT for App auth (`iss=appID`,
// `iat=now-skew`, `exp=now+AppJWTTTL`). `now` is a parameter so tests are
// hermetic.
//
// GitHub's required claim set is tiny — we mint the JWT directly rather than
// pulling in a JWT library. The header and payload are stable JSON literals
// so the signature is deterministic given (appID, key, now).
func NewAppJWT(appID int64, key *rsa.PrivateKey, now time.Time) (string, error) {
	if key == nil {
		return "", errors.New("githubapp: nil RSA key")
	}
	if appID <= 0 {
		return "", fmt.Errorf("githubapp: invalid app_id %d", appID)
	}

	headerJSON := []byte(`{"alg":"RS256","typ":"JWT"}`)
	payload := map[string]any{
		"iat": now.Add(-jwtSkew).Unix(),
		"exp": now.Add(AppJWTTTL).Unix(),
		"iss": appID,
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}

	enc := base64.RawURLEncoding
	signingInput := enc.EncodeToString(headerJSON) + "." + enc.EncodeToString(payloadJSON)

	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signingInput + "." + enc.EncodeToString(sig), nil
}
