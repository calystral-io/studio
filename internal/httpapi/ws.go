// WebSocket scaffold (contract section 6): GET /api/v1/ws upgrades, authenticates
// in-handshake (token via `Sec-WebSocket-Protocol: bearer,<token>` or
// `?access_token=`), heartbeats with ping/pong, frames messages as a typed
// envelope, and sends one real `hello` message on connect. Live data streams
// land later; the framing + auth + tests ship now so the seam is real.
//
// Library choice: github.com/coder/websocket (the maintained successor to
// nhooyr.io/websocket). Picked for its context-first API, built-in Ping and
// CloseRead control-frame handling, and zero cgo - a clean fit for a Go BFF.
package httpapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/calystral-io/studio/internal/apierr"
)

// wsHeartbeatInterval is how often the server pings idle clients.
const wsHeartbeatInterval = 15 * time.Second

// wsEnvelope is the typed message frame exchanged over the socket.
type wsEnvelope struct {
	Type    string `json:"type"`
	Payload any    `json:"payload"`
}

// helloPayload is the single real message PR1 emits on connect.
type helloPayload struct {
	Principal  principalView `json:"principal"`
	ServerTime string        `json:"server_time"`
}

type principalView struct {
	TenantID string   `json:"tenant_id"`
	UserID   string   `json:"user_id"`
	Roles    []string `json:"roles"`
}

// handleWS performs the auth handshake, upgrades, sends hello, and heartbeats.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	reqID := requestIDOf(r)

	// Resolve the token from the WS-specific transports and reuse the standard
	// Authenticator by presenting it as a bearer header. An empty token yields
	// missing_token; an unrecognized one yields invalid_token.
	if token := resolveWSToken(r); token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	principal, err := s.auth.Authenticate(r)
	if err != nil {
		// Not yet upgraded: render the typed 401 envelope over plain HTTP.
		apierr.Write(w, reqID, err)
		return
	}

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:   []string{"bearer"},
		OriginPatterns: s.originPatterns,
	})
	if err != nil {
		// Accept already wrote an HTTP error response on failure.
		s.logger.Error("ws accept failed", "request_id", reqID, "error", err.Error())
		return
	}
	defer c.CloseNow()

	ctx := r.Context()

	// Send the hello message before delegating reads to CloseRead.
	hello := wsEnvelope{Type: "hello", Payload: helloPayload{
		Principal: principalView{
			TenantID: principal.TenantID,
			UserID:   principal.UserID,
			Roles:    principal.Roles,
		},
		ServerTime: time.Now().UTC().Format(time.RFC3339),
	}}
	writeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	if err := wsjson.Write(writeCtx, c, hello); err != nil {
		cancel()
		return
	}
	cancel()

	// CloseRead handles inbound control frames (auto-pong) and signals closure.
	ctx = c.CloseRead(ctx)

	ticker := time.NewTicker(wsHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			c.Close(websocket.StatusNormalClosure, "bye")
			return
		case <-ticker.C:
			pingCtx, pcancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.Ping(pingCtx)
			pcancel()
			if err != nil {
				return
			}
		}
	}
}

// resolveWSToken extracts the bearer token from the WS subprotocol header or the
// access_token query parameter. Returns "" when neither carries a token.
func resolveWSToken(r *http.Request) string {
	if proto := r.Header.Get("Sec-WebSocket-Protocol"); proto != "" {
		parts := strings.Split(proto, ",")
		if len(parts) >= 2 && strings.EqualFold(strings.TrimSpace(parts[0]), "bearer") {
			if tok := strings.TrimSpace(parts[1]); tok != "" {
				return tok
			}
		}
	}
	return strings.TrimSpace(r.URL.Query().Get("access_token"))
}
