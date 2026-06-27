package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"

	"github.com/calystral-io/studio/internal/auth"
)

type ctxKey int

const (
	ctxKeyRequestID ctxKey = iota
	ctxKeyPrincipal
)

// RequestIDHeader is the correlation header echoed on every response.
const RequestIDHeader = "X-Request-Id"

// withRequestID stores the request id in the context.
func withRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID, id)
}

// requestIDFrom returns the request id stored in the context, or "".
func requestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return v
	}
	return ""
}

// withPrincipal stores the resolved principal in the context.
func withPrincipal(ctx context.Context, p *auth.Principal) context.Context {
	return context.WithValue(ctx, ctxKeyPrincipal, p)
}

// principalFrom returns the resolved principal, or nil if unauthenticated.
func principalFrom(ctx context.Context) *auth.Principal {
	if v, ok := ctx.Value(ctxKeyPrincipal).(*auth.Principal); ok {
		return v
	}
	return nil
}

// requestIDOf returns the request id for the request's context.
func requestIDOf(r *http.Request) string { return requestIDFrom(r.Context()) }

// newRequestID mints an opaque correlation id.
func newRequestID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req_0000000000000000000000"
	}
	return "req_" + hex.EncodeToString(b[:])
}
