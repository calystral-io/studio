// Package httpapi is the BFF HTTP edge: a chi router with request-id,
// structured slog logging, panic recovery, CORS, and mock auth middleware,
// serving the contract's health/version/identity, anchors, ledgers, and
// WebSocket surfaces.
package httpapi

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/calystral-io/studio/internal/apierr"
	"github.com/calystral-io/studio/internal/auth"
	"github.com/calystral-io/studio/internal/coreclient"
)

// Options carries edge configuration not owned by a dependency.
type Options struct {
	CORSOrigins []string
}

// Server wires the BFF dependencies into an http.Handler.
type Server struct {
	auth           auth.Authenticator
	core           coreclient.CoreClient
	logger         *slog.Logger
	originPatterns []string
	handler        http.Handler
}

// New builds a Server and its routed handler from injected dependencies.
func New(authn auth.Authenticator, core coreclient.CoreClient, logger *slog.Logger, opts Options) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{auth: authn, core: core, logger: logger, originPatterns: originPatterns(opts.CORSOrigins)}
	s.handler = s.routes(opts)
	return s
}

// originPatterns maps allowed CORS origins to host patterns for the WebSocket
// same-origin check (the scheme is stripped; coder/websocket matches on host).
func originPatterns(origins []string) []string {
	out := make([]string, 0, len(origins))
	for _, o := range origins {
		host := o
		if i := strings.Index(host, "://"); i >= 0 {
			host = host[i+3:]
		}
		if host != "" {
			out = append(out, host)
		}
	}
	return out
}

// Handler returns the composed http.Handler.
func (s *Server) Handler() http.Handler { return s.handler }

// ServeHTTP lets the Server satisfy http.Handler directly.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

func (s *Server) routes(opts Options) http.Handler {
	r := chi.NewRouter()

	r.Use(requestIDMiddleware)
	r.Use(recoverMiddleware(s.logger))
	r.Use(loggingMiddleware(s.logger))
	r.Use(corsMiddleware(opts.CORSOrigins))

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		apierr.Write(w, requestIDOf(r), apierr.NotFound(r.URL.Path))
	})
	r.MethodNotAllowed(func(w http.ResponseWriter, r *http.Request) {
		apierr.Write(w, requestIDOf(r), apierr.NotFound(r.URL.Path))
	})

	// Unauthenticated infra probes.
	r.Get("/healthz", s.handleHealthz)
	r.Get("/readyz", s.handleReadyz)

	r.Route("/api/v1", func(r chi.Router) {
		// Unauthenticated identity surface.
		r.Get("/version", s.handleVersion)

		// WebSocket authenticates in-handshake (token via subprotocol or query),
		// so it sits outside the Authorization-header auth middleware.
		r.Get("/ws", s.handleWS)

		// Authenticated surfaces.
		r.Group(func(r chi.Router) {
			r.Use(authMiddleware(s.auth))
			r.Get("/me", s.handleMe)
			r.Get("/anchors", s.handleAnchors)
			r.Get("/ledgers", s.handleLedgers)
			r.Get("/ledgers/{name}/entries", s.handleLedgerEntries)
		})
	})

	return r
}
