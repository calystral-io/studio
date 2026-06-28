package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/calystral-io/studio/internal/apierr"
)

func TestMockAuthenticate(t *testing.T) {
	tests := []struct {
		name       string
		authHeader string
		setHeader  bool
		wantErr    apierr.Code
		wantUser   string
		wantRoles  []string
	}{
		{
			name:      "missing header",
			setHeader: false,
			wantErr:   apierr.CodeMissingToken,
		},
		{
			name:       "empty header value",
			authHeader: "",
			setHeader:  true,
			wantErr:    apierr.CodeMissingToken,
		},
		{
			name:       "non bearer scheme",
			authHeader: "Basic abc",
			setHeader:  true,
			wantErr:    apierr.CodeInvalidToken,
		},
		{
			name:       "bearer with empty token",
			authHeader: "Bearer ",
			setHeader:  true,
			wantErr:    apierr.CodeInvalidToken,
		},
		{
			name:       "unrecognized token",
			authHeader: "Bearer nope",
			setHeader:  true,
			wantErr:    apierr.CodeInvalidToken,
		},
		{
			name:       "valid admin token",
			authHeader: "Bearer mock-admin-token",
			setHeader:  true,
			wantUser:   "admin@demo",
			wantRoles:  []string{"admin", "reader", "writer"},
		},
		{
			name:       "valid writer token",
			authHeader: "Bearer mock-writer-token",
			setHeader:  true,
			wantUser:   "writer@demo",
			wantRoles:  []string{"writer", "reader"},
		},
		{
			name:       "valid reader token case-insensitive scheme",
			authHeader: "bearer mock-reader-token",
			setHeader:  true,
			wantUser:   "reader@demo",
			wantRoles:  []string{"reader"},
		},
	}

	var a MockAuthenticator
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
			if tc.setHeader {
				r.Header.Set("Authorization", tc.authHeader)
			}
			p, err := a.Authenticate(r)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error %q, got principal %+v", tc.wantErr, p)
				}
				ae, ok := err.(*apierr.APIError)
				if !ok {
					t.Fatalf("error is not *APIError: %T", err)
				}
				if ae.Code != tc.wantErr {
					t.Fatalf("code = %q, want %q", ae.Code, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if p.UserID != tc.wantUser {
				t.Errorf("user = %q, want %q", p.UserID, tc.wantUser)
			}
			if p.TenantID != "demo-tenant" {
				t.Errorf("tenant = %q, want demo-tenant", p.TenantID)
			}
			if len(p.Roles) != len(tc.wantRoles) {
				t.Fatalf("roles = %v, want %v", p.Roles, tc.wantRoles)
			}
			for i, role := range tc.wantRoles {
				if p.Roles[i] != role {
					t.Errorf("roles[%d] = %q, want %q", i, p.Roles[i], role)
				}
			}
			if p.AuditSessionID == "" {
				t.Error("audit session id must be set")
			}
		})
	}
}

func TestPrincipalHasRole(t *testing.T) {
	p := &Principal{Roles: []string{"reader"}}
	if !p.HasRole("reader") {
		t.Error("expected reader role")
	}
	if p.HasRole("admin") {
		t.Error("did not expect admin role")
	}
	var nilP *Principal
	if nilP.HasRole("reader") {
		t.Error("nil principal must not have roles")
	}
}
