// Package auth resolves the request principal behind a stable interface so the
// PR1 mock token map can be swapped for a real Nexus forwarder without touching
// the rest of the BFF or the UI contract. It also mints the dev
// x-calystral-principal JWT the gRPC Core adapter forwards.
package auth

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/calystral-io/studio/internal/apierr"
)

// Principal is the resolved identity for a request (contract section 2).
type Principal struct {
	TenantID       string
	UserID         string
	Roles          []string
	AuditSessionID string
}

// HasRole reports whether the principal carries the named role.
func (p *Principal) HasRole(role string) bool {
	if p == nil {
		return false
	}
	for _, r := range p.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// Authenticator resolves the request principal or returns a *apierr.APIError
// (mapped to 401). Implementations: MockAuthenticator (PR1), a Nexus forwarder
// (later).
type Authenticator interface {
	Authenticate(r *http.Request) (*Principal, error)
}

// mockToken is a static principal template keyed by bearer token.
type mockToken struct {
	tenantID string
	userID   string
	roles    []string
}

// mockTokens is the contract section 2 token map.
var mockTokens = map[string]mockToken{
	"mock-admin-token":  {tenantID: "demo-tenant", userID: "admin@demo", roles: []string{"admin", "reader"}},
	"mock-reader-token": {tenantID: "demo-tenant", userID: "reader@demo", roles: []string{"reader"}},
}

// MockAuthenticator implements Authenticator with the static PR1 token map.
type MockAuthenticator struct{}

// Authenticate resolves the bearer token to a principal. Missing header ->
// missing_token; unrecognized token -> invalid_token.
func (MockAuthenticator) Authenticate(r *http.Request) (*Principal, error) {
	token, err := BearerToken(r)
	if err != nil {
		return nil, err
	}
	mt, ok := mockTokens[token]
	if !ok {
		return nil, apierr.InvalidToken()
	}
	roles := make([]string, len(mt.roles))
	copy(roles, mt.roles)
	return &Principal{
		TenantID:       mt.tenantID,
		UserID:         mt.userID,
		Roles:          roles,
		AuditSessionID: newAuditSessionID(),
	}, nil
}

// BearerToken extracts the bearer token from the Authorization header. An
// absent header is missing_token; a malformed or empty value is invalid_token.
func BearerToken(r *http.Request) (string, error) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", apierr.MissingToken()
	}
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return "", apierr.InvalidToken()
	}
	token := strings.TrimSpace(h[len(prefix):])
	if token == "" {
		return "", apierr.InvalidToken()
	}
	return token, nil
}

// newAuditSessionID mints a per-session correlation id for the audit trail.
func newAuditSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read failing is catastrophic; fall back to a fixed dev marker
		// rather than panicking on the request path.
		return "as_dev0000000000000000000000000000"
	}
	return "as_" + hex.EncodeToString(b[:])
}
