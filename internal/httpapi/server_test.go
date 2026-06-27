package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/calystral-io/studio/internal/auth"
	"github.com/calystral-io/studio/internal/coreclient"
	"github.com/calystral-io/studio/internal/corepb/querypb"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newFixtureServer() *Server {
	return New(auth.MockAuthenticator{}, coreclient.NewFixture(), quietLogger(),
		Options{CORSOrigins: []string{"http://localhost:5173"}})
}

// do issues a request against the routed handler and returns the recorder.
func do(t *testing.T, s *Server, method, target, token string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
}

type errEnvelope struct {
	Error struct {
		Code      string         `json:"code"`
		Params    map[string]any `json:"params"`
		Message   string         `json:"message"`
		RequestID string         `json:"request_id"`
	} `json:"error"`
}

func TestHealthz(t *testing.T) {
	s := newFixtureServer()
	rec := do(t, s, http.MethodGet, "/healthz", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body map[string]string
	decode(t, rec, &body)
	if body["status"] != "ok" {
		t.Errorf("body = %v", body)
	}
	if rec.Header().Get(RequestIDHeader) == "" {
		t.Error("missing X-Request-Id header")
	}
}

func TestReadyzFixture(t *testing.T) {
	s := newFixtureServer()
	rec := do(t, s, http.MethodGet, "/readyz", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	decode(t, rec, &body)
	if body.Status != "ready" || body.Checks["core"] != "skip" {
		t.Errorf("body = %+v", body)
	}
}

func TestVersion(t *testing.T) {
	s := newFixtureServer()
	rec := do(t, s, http.MethodGet, "/api/v1/version", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body map[string]string
	decode(t, rec, &body)
	if body["service"] != "studio" {
		t.Errorf("service = %q", body["service"])
	}
	if body["go"] == "" {
		t.Error("go version must be populated")
	}
	if _, ok := body["build_time"]; !ok {
		t.Error("build_time key must be present (zero-value safe)")
	}
}

func TestMe(t *testing.T) {
	s := newFixtureServer()

	t.Run("missing token", func(t *testing.T) {
		rec := do(t, s, http.MethodGet, "/api/v1/me", "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d", rec.Code)
		}
		var env errEnvelope
		decode(t, rec, &env)
		if env.Error.Code != "/errors/auth/missing_token" {
			t.Errorf("code = %q", env.Error.Code)
		}
		if env.Error.RequestID == "" {
			t.Error("request_id missing in envelope")
		}
	})

	t.Run("invalid token", func(t *testing.T) {
		rec := do(t, s, http.MethodGet, "/api/v1/me", "bogus")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d", rec.Code)
		}
		var env errEnvelope
		decode(t, rec, &env)
		if env.Error.Code != "/errors/auth/invalid_token" {
			t.Errorf("code = %q", env.Error.Code)
		}
	})

	t.Run("valid admin", func(t *testing.T) {
		rec := do(t, s, http.MethodGet, "/api/v1/me", "mock-admin-token")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		var body struct {
			TenantID string   `json:"tenant_id"`
			UserID   string   `json:"user_id"`
			Roles    []string `json:"roles"`
		}
		decode(t, rec, &body)
		if body.UserID != "admin@demo" || body.TenantID != "demo-tenant" {
			t.Errorf("body = %+v", body)
		}
		if len(body.Roles) != 2 {
			t.Errorf("roles = %v", body.Roles)
		}
	})
}

func TestAnchorsHappyPathAndPagination(t *testing.T) {
	s := newFixtureServer()

	type anchorsBody struct {
		Items  []coreclient.AnchorDTO `json:"items"`
		Page   coreclient.Page        `json:"page"`
		Source string                 `json:"source"`
	}

	cursor := ""
	total := 0
	pages := 0
	for {
		target := "/api/v1/anchors?page_size=50"
		if cursor != "" {
			target += "&cursor=" + cursor
		}
		rec := do(t, s, http.MethodGet, target, "mock-reader-token")
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d status = %d body=%s", pages, rec.Code, rec.Body.String())
		}
		var body anchorsBody
		decode(t, rec, &body)
		pages++
		if body.Source != "fixture" {
			t.Errorf("source = %q, want fixture", body.Source)
		}
		if body.Page.TotalEstimate != 142 {
			t.Errorf("total_estimate = %d, want 142", body.Page.TotalEstimate)
		}
		total += len(body.Items)
		if !body.Page.HasMore {
			if body.Page.NextCursor != nil {
				t.Error("next_cursor must be null on last page")
			}
			break
		}
		if body.Page.NextCursor == nil {
			t.Fatal("has_more true but next_cursor nil")
		}
		cursor = *body.Page.NextCursor
		if pages > 50 {
			t.Fatal("pagination did not terminate")
		}
	}
	if total != 142 {
		t.Fatalf("walked %d anchors, want 142", total)
	}
}

func TestAnchorsTypeFilter(t *testing.T) {
	s := newFixtureServer()
	rec := do(t, s, http.MethodGet, "/api/v1/anchors?type=Customer&page_size=200", "mock-reader-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Items []coreclient.AnchorDTO `json:"items"`
		Page  coreclient.Page        `json:"page"`
	}
	decode(t, rec, &body)
	if body.Page.TotalEstimate != 30 {
		t.Errorf("Customer total = %d, want 30", body.Page.TotalEstimate)
	}
	for _, a := range body.Items {
		if a.Type != "Customer" {
			t.Errorf("unexpected type %q", a.Type)
		}
	}
}

func TestAnchorsValidation(t *testing.T) {
	s := newFixtureServer()
	tests := []struct {
		name     string
		target   string
		wantCode string
	}{
		{"page_size too large", "/api/v1/anchors?page_size=999", "/errors/validation/page_size_out_of_range"},
		{"page_size zero", "/api/v1/anchors?page_size=0", "/errors/validation/page_size_out_of_range"},
		{"page_size non-integer", "/api/v1/anchors?page_size=abc", "/errors/validation/page_size_out_of_range"},
		{"bad cursor", "/api/v1/anchors?cursor=%21%21%21", "/errors/validation/invalid_cursor"},
		{"bad as_of", "/api/v1/anchors?as_of=not-a-time", "/errors/validation/invalid_as_of"},
		{"bad system_as_of", "/api/v1/anchors?system_as_of=not-a-time", "/errors/validation/invalid_system_as_of"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, s, http.MethodGet, tc.target, "mock-reader-token")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
			}
			var env errEnvelope
			decode(t, rec, &env)
			if env.Error.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", env.Error.Code, tc.wantCode)
			}
		})
	}
}

func TestAnchorsRequiresAuth(t *testing.T) {
	s := newFixtureServer()
	rec := do(t, s, http.MethodGet, "/api/v1/anchors", "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
}

// rolelessAuth resolves a principal lacking the reader role, to exercise 403.
type rolelessAuth struct{}

func (rolelessAuth) Authenticate(*http.Request) (*auth.Principal, error) {
	return &auth.Principal{TenantID: "demo-tenant", UserID: "noone@demo", Roles: []string{"guest"}}, nil
}

func TestAnchorsForbiddenWithoutReader(t *testing.T) {
	s := New(rolelessAuth{}, coreclient.NewFixture(), quietLogger(), Options{})
	rec := do(t, s, http.MethodGet, "/api/v1/anchors", "any")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d", rec.Code)
	}
	var env errEnvelope
	decode(t, rec, &env)
	if env.Error.Code != "/errors/auth/forbidden" {
		t.Errorf("code = %q", env.Error.Code)
	}
}

func TestNotFoundEnvelope(t *testing.T) {
	s := newFixtureServer()
	rec := do(t, s, http.MethodGet, "/api/v1/nope", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d", rec.Code)
	}
	var env errEnvelope
	decode(t, rec, &env)
	if env.Error.Code != "/errors/not_found" {
		t.Errorf("code = %q", env.Error.Code)
	}
}

func TestCORSAllowedOrigin(t *testing.T) {
	s := newFixtureServer()
	r := httptest.NewRequest(http.MethodOptions, "/api/v1/anchors", nil)
	r.Header.Set("Origin", "http://localhost:5173")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d", rec.Code)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Errorf("allow-origin = %q", got)
	}
}

func TestCORSDisallowedOrigin(t *testing.T) {
	s := newFixtureServer()
	r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	r.Header.Set("Origin", "http://evil.test")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("disallowed origin should not be echoed, got %q", got)
	}
}

// --- gRPC source 501 path ---------------------------------------------------

type unimplementedQuery struct {
	querypb.UnimplementedQueryServiceServer
}

func (unimplementedQuery) Query(context.Context, *querypb.QueryRequest) (*querypb.QueryResponse, error) {
	return nil, status.Error(codes.Unimplemented, "cvm opcode gap")
}

func TestAnchorsGRPCSourceReturns501(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	grpcSrv := grpc.NewServer()
	querypb.RegisterQueryServiceServer(grpcSrv, unimplementedQuery{})
	go func() { _ = grpcSrv.Serve(lis) }()
	defer grpcSrv.Stop()

	signer, err := auth.NewPrincipalSigner("")
	if err != nil {
		t.Fatal(err)
	}
	core, err := coreclient.NewGRPCClient(lis.Addr().String(), signer)
	if err != nil {
		t.Fatal(err)
	}
	defer core.Close()

	s := New(auth.MockAuthenticator{}, core, quietLogger(), Options{})
	rec := do(t, s, http.MethodGet, "/api/v1/anchors", "mock-reader-token")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var env errEnvelope
	decode(t, rec, &env)
	if env.Error.Code != "/errors/upstream/unimplemented" {
		t.Errorf("code = %q", env.Error.Code)
	}
	if env.Error.Params["surface"] != "anchors" {
		t.Errorf("surface = %v", env.Error.Params["surface"])
	}
}
