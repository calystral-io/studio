package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestMintProducesValidEdDSAJWT(t *testing.T) {
	signer, err := NewPrincipalSigner("")
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	if !signer.DevGenerated() {
		t.Error("empty key should generate a dev key")
	}

	p := &Principal{
		TenantID:       "demo-tenant",
		UserID:         "admin@demo",
		Roles:          []string{"admin", "reader"},
		AuditSessionID: "as_abc123",
	}
	tokenStr, err := signer.Mint(p)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	pub := signer.Public()
	parsed, err := jwt.ParseWithClaims(tokenStr, &principalClaims{}, func(tok *jwt.Token) (any, error) {
		if tok.Method.Alg() != "EdDSA" {
			t.Fatalf("alg = %q, want EdDSA", tok.Method.Alg())
		}
		return pub, nil
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !parsed.Valid {
		t.Fatal("token not valid")
	}

	claims, ok := parsed.Claims.(*principalClaims)
	if !ok {
		t.Fatalf("claims type %T", parsed.Claims)
	}
	if claims.TenantID != p.TenantID {
		t.Errorf("tenant_id = %q, want %q", claims.TenantID, p.TenantID)
	}
	if claims.UserID != p.UserID {
		t.Errorf("user_id = %q, want %q", claims.UserID, p.UserID)
	}
	if claims.AuditSessionID != p.AuditSessionID {
		t.Errorf("audit_session_id = %q, want %q", claims.AuditSessionID, p.AuditSessionID)
	}
	if len(claims.Roles) != 2 || claims.Roles[0] != "admin" || claims.Roles[1] != "reader" {
		t.Errorf("roles = %v", claims.Roles)
	}
	if claims.Issuer != PrincipalIssuer {
		t.Errorf("iss = %q, want %q", claims.Issuer, PrincipalIssuer)
	}
	if claims.ExpiresAt == nil || !claims.ExpiresAt.After(time.Now()) {
		t.Error("exp must be in the future")
	}
}

func TestMintRejectsTamper(t *testing.T) {
	signer, _ := NewPrincipalSigner("")
	tokenStr, err := signer.Mint(&Principal{TenantID: "t", UserID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	// Verify with a different key must fail.
	otherPub, _, _ := ed25519.GenerateKey(rand.Reader)
	_, err = jwt.ParseWithClaims(tokenStr, &principalClaims{}, func(*jwt.Token) (any, error) {
		return otherPub, nil
	})
	if err == nil {
		t.Fatal("expected verification failure with wrong key")
	}
}

func TestSignerFromInlineSeed(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	enc := base64.StdEncoding.EncodeToString(seed)

	signer, err := NewPrincipalSigner(enc)
	if err != nil {
		t.Fatalf("new signer from seed: %v", err)
	}
	if signer.DevGenerated() {
		t.Error("explicit seed must not be marked generated")
	}
	want := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	if !signer.Public().Equal(want) {
		t.Error("public key does not match seed")
	}
}

func TestSignerFromFileSeed(t *testing.T) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(255 - i)
	}
	enc := base64.RawURLEncoding.EncodeToString(seed)
	dir := t.TempDir()
	path := filepath.Join(dir, "dev.key")
	if err := os.WriteFile(path, []byte(enc), 0o600); err != nil {
		t.Fatal(err)
	}
	signer, err := NewPrincipalSigner(path)
	if err != nil {
		t.Fatalf("new signer from file: %v", err)
	}
	want := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
	if !signer.Public().Equal(want) {
		t.Error("file-loaded public key mismatch")
	}
}

func TestSignerRejectsBadSeed(t *testing.T) {
	if _, err := NewPrincipalSigner("not-base64!!!"); err == nil {
		t.Error("expected error for bad seed")
	}
	short := base64.StdEncoding.EncodeToString([]byte("too-short"))
	if _, err := NewPrincipalSigner(short); err == nil {
		t.Error("expected error for short seed")
	}
}
