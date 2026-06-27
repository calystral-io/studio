// EdDSA (Ed25519) minting of the x-calystral-principal JWT that the gRPC Core
// adapter forwards. PR1 uses a dev-only signing key (generated when unset);
// when Nexus lands the BFF will forward the real inbound JWT unchanged instead
// of minting one here.
package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// PrincipalIssuer is the iss claim value for BFF-minted dev principals.
const PrincipalIssuer = "studio-bff-dev"

// principalClaims is Core's expected claim set for x-calystral-principal.
type principalClaims struct {
	TenantID       string   `json:"tenant_id"`
	UserID         string   `json:"user_id"`
	Roles          []string `json:"roles"`
	AuditSessionID string   `json:"audit_session_id"`
	jwt.RegisteredClaims
}

// PrincipalSigner mints short-lived EdDSA principal JWTs for Core calls.
type PrincipalSigner struct {
	key ed25519.PrivateKey
	ttl time.Duration
	now func() time.Time
	// devGenerated records whether the key was generated (no key configured).
	devGenerated bool
}

// NewPrincipalSigner builds a signer from the configured dev signing key. The
// key value may be empty (a dev keypair is generated), a filesystem path to a
// 32-byte seed, or an inline standard-base64 32-byte seed. A generated key is
// clearly dev-only and never persisted.
func NewPrincipalSigner(devSigningKey string) (*PrincipalSigner, error) {
	key, generated, err := loadOrGenerateKey(devSigningKey)
	if err != nil {
		return nil, err
	}
	return &PrincipalSigner{key: key, ttl: 5 * time.Minute, now: time.Now, devGenerated: generated}, nil
}

// DevGenerated reports whether the signer is using an ephemeral generated key.
func (s *PrincipalSigner) DevGenerated() bool { return s.devGenerated }

// Public returns the verifying key, useful for tests and key publication.
func (s *PrincipalSigner) Public() ed25519.PublicKey {
	return s.key.Public().(ed25519.PublicKey)
}

// Mint produces a signed EdDSA JWT carrying the principal claims Core expects.
func (s *PrincipalSigner) Mint(p *Principal) (string, error) {
	if p == nil {
		return "", fmt.Errorf("mint: nil principal")
	}
	now := s.now()
	claims := principalClaims{
		TenantID:       p.TenantID,
		UserID:         p.UserID,
		Roles:          p.Roles,
		AuditSessionID: p.AuditSessionID,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    PrincipalIssuer,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(s.ttl)),
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	return tok.SignedString(s.key)
}

// loadOrGenerateKey resolves the configured dev key or generates one.
func loadOrGenerateKey(devSigningKey string) (ed25519.PrivateKey, bool, error) {
	devSigningKey = strings.TrimSpace(devSigningKey)
	if devSigningKey == "" {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, false, fmt.Errorf("generate dev signing key: %w", err)
		}
		return priv, true, nil
	}
	// Filesystem path takes precedence when the file exists.
	if raw, err := os.ReadFile(devSigningKey); err == nil {
		seed, derr := decodeSeed(strings.TrimSpace(string(raw)))
		if derr != nil {
			return nil, false, fmt.Errorf("dev signing key file %q: %w", devSigningKey, derr)
		}
		return ed25519.NewKeyFromSeed(seed), false, nil
	}
	// Otherwise treat the value itself as an inline base64 seed.
	seed, err := decodeSeed(devSigningKey)
	if err != nil {
		return nil, false, fmt.Errorf("inline dev signing key: %w", err)
	}
	return ed25519.NewKeyFromSeed(seed), false, nil
}

// decodeSeed accepts standard or url base64 and validates a 32-byte Ed25519 seed.
func decodeSeed(s string) ([]byte, error) {
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			if len(b) != ed25519.SeedSize {
				return nil, fmt.Errorf("seed must be %d bytes, got %d", ed25519.SeedSize, len(b))
			}
			return b, nil
		}
	}
	return nil, fmt.Errorf("not valid base64")
}
