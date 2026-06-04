package githubapp

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

// generateTestKey returns a small RSA key for tests. 2048 bits is plenty for
// signature verification; the bigger sizes used by real Apps just slow tests
// down.
func generateTestKey(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	k, err := rsa.GenerateKey(testRand(), 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	return k
}

func TestParseRSAPrivateKey(t *testing.T) {
	k := generateTestKey(t)

	t.Run("PKCS1", func(t *testing.T) {
		pemBytes := pem.EncodeToMemory(&pem.Block{
			Type:  "RSA PRIVATE KEY",
			Bytes: x509.MarshalPKCS1PrivateKey(k),
		})
		got, err := ParseRSAPrivateKey(pemBytes)
		if err != nil {
			t.Fatalf("PKCS1: %v", err)
		}
		if got.N.Cmp(k.N) != 0 {
			t.Fatal("modulus mismatch")
		}
	})

	t.Run("PKCS8", func(t *testing.T) {
		der, err := x509.MarshalPKCS8PrivateKey(k)
		if err != nil {
			t.Fatal(err)
		}
		pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
		got, err := ParseRSAPrivateKey(pemBytes)
		if err != nil {
			t.Fatalf("PKCS8: %v", err)
		}
		if got.N.Cmp(k.N) != 0 {
			t.Fatal("modulus mismatch")
		}
	})

	t.Run("garbage", func(t *testing.T) {
		if _, err := ParseRSAPrivateKey([]byte("not a pem")); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestNewAppJWT(t *testing.T) {
	k := generateTestKey(t)
	now := time.Unix(1_700_000_000, 0)
	appID := int64(12345)

	tok, err := NewAppJWT(appID, k, now)
	if err != nil {
		t.Fatalf("NewAppJWT: %v", err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("want 3 segments, got %d", len(parts))
	}

	// Claim shape: iat = now-skew, exp = now+TTL, iss = appID.
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var claims struct {
		Iat int64 `json:"iat"`
		Exp int64 `json:"exp"`
		Iss int64 `json:"iss"`
	}
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if claims.Iss != appID {
		t.Errorf("iss = %d, want %d", claims.Iss, appID)
	}
	wantIat := now.Add(-jwtSkew).Unix()
	if claims.Iat != wantIat {
		t.Errorf("iat = %d, want %d", claims.Iat, wantIat)
	}
	wantExp := now.Add(AppJWTTTL).Unix()
	if claims.Exp != wantExp {
		t.Errorf("exp = %d, want %d", claims.Exp, wantExp)
	}

	// Signature verifies against the same key.
	signingInput := parts[0] + "." + parts[1]
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		t.Fatalf("decode sig: %v", err)
	}
	if err := rsa.VerifyPKCS1v15(&k.PublicKey, crypto.SHA256, digest[:], sig); err != nil {
		t.Fatalf("signature verify: %v", err)
	}
}

func TestNewAppJWTInvalid(t *testing.T) {
	k := generateTestKey(t)
	if _, err := NewAppJWT(0, k, time.Now()); err == nil {
		t.Fatal("expected error for app_id=0")
	}
	if _, err := NewAppJWT(1, nil, time.Now()); err == nil {
		t.Fatal("expected error for nil key")
	}
}

func TestTokenValid(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	cases := []struct {
		name  string
		token *Token
		want  bool
	}{
		{"nil", nil, false},
		{"empty", &Token{ExpiresAt: now.Add(time.Hour)}, false},
		{"fresh", &Token{Token: "x", ExpiresAt: now.Add(time.Hour)}, true},
		{"within buffer", &Token{Token: "x", ExpiresAt: now.Add(CacheRefreshBuffer - time.Second)}, false},
		{"expired", &Token{Token: "x", ExpiresAt: now.Add(-time.Hour)}, false},
		{"at buffer edge", &Token{Token: "x", ExpiresAt: now.Add(CacheRefreshBuffer)}, false},
		{"just past buffer", &Token{Token: "x", ExpiresAt: now.Add(CacheRefreshBuffer + time.Second)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.token.Valid(now); got != tc.want {
				t.Errorf("Valid() = %v, want %v", got, tc.want)
			}
		})
	}
}
