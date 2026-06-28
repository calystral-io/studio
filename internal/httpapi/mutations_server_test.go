package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// doBody issues a request with a JSON body and returns the recorder.
func doBody(t *testing.T, s *Server, method, target, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, r)
	return rec
}

func TestCreateAnchorHappyPathAndReadback(t *testing.T) {
	s := newFixtureServer()

	rec := doBody(t, s, http.MethodPost, "/api/v1/anchors", "mock-writer-token",
		`{"id":"anchor_made_0001","type":"Service","label":"Billing","properties":{"tier":"gold"}}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Anchor struct {
			ID       string  `json:"id"`
			Label    string  `json:"label"`
			SystemTo *string `json:"system_to"`
		} `json:"anchor"`
		Source string `json:"source"`
	}
	decode(t, rec, &body)
	if body.Anchor.ID != "anchor_made_0001" || body.Anchor.Label != "Billing" || body.Anchor.SystemTo != nil {
		t.Fatalf("created anchor = %+v", body.Anchor)
	}
	if body.Source != "fixture" {
		t.Errorf("source = %q", body.Source)
	}

	// Honest statefulness: a follow-up read reflects the write.
	rec = do(t, s, http.MethodGet, "/api/v1/anchors/anchor_made_0001/history", "mock-reader-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("history status = %d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Billing") {
		t.Error("created anchor not reflected in history read-back")
	}
}

func TestCorrectAndCloseReadback(t *testing.T) {
	s := newFixtureServer()

	// Correct an existing anchor, then verify history shows the supersession.
	rec := doBody(t, s, http.MethodPost, "/api/v1/anchors/anchor_employee_0001/corrections",
		"mock-writer-token", `{"label":"Renamed"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("correct status = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = do(t, s, http.MethodGet, "/api/v1/anchors/anchor_employee_0001/history", "mock-reader-token")
	if !strings.Contains(rec.Body.String(), "Renamed") {
		t.Error("correction not reflected in history")
	}

	// Close it; the close + valid_to surface in a diff against now vs the close.
	rec = doBody(t, s, http.MethodPost, "/api/v1/anchors/anchor_employee_0001/close", "mock-writer-token", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("close status = %d body=%s", rec.Code, rec.Body.String())
	}
	var closeBody struct {
		Anchor struct {
			Closed  bool    `json:"closed"`
			ValidTo *string `json:"valid_to"`
		} `json:"anchor"`
	}
	decode(t, rec, &closeBody)
	if !closeBody.Anchor.Closed || closeBody.Anchor.ValidTo == nil {
		t.Errorf("closed anchor = %+v", closeBody.Anchor)
	}

	// Closing again -> 400 invalid_request.
	rec = doBody(t, s, http.MethodPost, "/api/v1/anchors/anchor_employee_0001/close", "mock-writer-token", `{}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("re-close status = %d", rec.Code)
	}
}

func TestMutationsRBAC(t *testing.T) {
	s := newFixtureServer()
	cases := []struct {
		name, method, target, body string
	}{
		{"create", http.MethodPost, "/api/v1/anchors", `{"id":"x","type":"T","label":"L"}`},
		{"correct", http.MethodPost, "/api/v1/anchors/anchor_employee_0001/corrections", `{"label":"L"}`},
		{"close", http.MethodPost, "/api/v1/anchors/anchor_employee_0001/close", `{}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// reader (no writer) -> 403.
			rec := doBody(t, s, c.method, c.target, "mock-reader-token", c.body)
			if rec.Code != http.StatusForbidden {
				t.Errorf("reader status = %d, want 403", rec.Code)
			}
			// missing token -> 401.
			rec = doBody(t, s, c.method, c.target, "", c.body)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("anon status = %d, want 401", rec.Code)
			}
		})
	}
}

func TestMutationsAdminAllowed(t *testing.T) {
	// admin is a writer superset and may mutate.
	s := newFixtureServer()
	rec := doBody(t, s, http.MethodPost, "/api/v1/anchors", "mock-admin-token", `{"id":"by_admin","type":"T","label":"L"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin create status = %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestMutationsValidation(t *testing.T) {
	s := newFixtureServer()
	tests := []struct {
		name, target, body, wantCode string
		wantStatus                   int
	}{
		{"missing id", "/api/v1/anchors", `{"type":"T","label":"L"}`, "/errors/validation/invalid_request", 400},
		{"missing type", "/api/v1/anchors", `{"id":"x","label":"L"}`, "/errors/validation/invalid_request", 400},
		{"malformed json", "/api/v1/anchors", `{not json`, "/errors/validation/invalid_request", 400},
		{"empty correction", "/api/v1/anchors/anchor_employee_0001/corrections", `{}`, "/errors/validation/invalid_request", 400},
		{"bad valid_from", "/api/v1/anchors", `{"id":"x","type":"T","label":"L","valid_from":"nope"}`, "/errors/validation/invalid_request", 400},
		{"unknown id correct", "/api/v1/anchors/anchor_nope/corrections", `{"label":"L"}`, "/errors/not_found", 404},
		{"unknown id close", "/api/v1/anchors/anchor_nope/close", `{}`, "/errors/not_found", 404},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := doBody(t, s, http.MethodPost, tc.target, "mock-writer-token", tc.body)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			var env errEnvelope
			decode(t, rec, &env)
			if env.Error.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", env.Error.Code, tc.wantCode)
			}
		})
	}
}

func TestMutationsConflicts(t *testing.T) {
	s := newFixtureServer()

	// Duplicate create -> 409 already_exists.
	_ = doBody(t, s, http.MethodPost, "/api/v1/anchors", "mock-writer-token", `{"id":"dup","type":"T","label":"L"}`)
	rec := doBody(t, s, http.MethodPost, "/api/v1/anchors", "mock-writer-token", `{"id":"dup","type":"T","label":"L"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("dup status = %d", rec.Code)
	}
	var env errEnvelope
	decode(t, rec, &env)
	if env.Error.Code != "/errors/conflict/already_exists" {
		t.Errorf("code = %q", env.Error.Code)
	}

	// Stale expected_lsn -> 409 precondition_failed.
	rec = doBody(t, s, http.MethodPost, "/api/v1/anchors/anchor_employee_0001/corrections",
		"mock-writer-token", `{"label":"L","expected_lsn":1}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("precondition status = %d body=%s", rec.Code, rec.Body.String())
	}
	decode(t, rec, &env)
	if env.Error.Code != "/errors/conflict/precondition_failed" {
		t.Errorf("code = %q", env.Error.Code)
	}
}

func TestMutationsGRPCSourceReturns501(t *testing.T) {
	s := newGRPCFixtureServer(t)
	cases := []struct {
		target, body, surface string
	}{
		{"/api/v1/anchors", `{"id":"x","type":"T","label":"L"}`, "anchor_create"},
		{"/api/v1/anchors/anchor_employee_0001/corrections", `{"label":"L"}`, "anchor_correct"},
		{"/api/v1/anchors/anchor_employee_0001/close", `{}`, "anchor_close"},
	}
	for _, c := range cases {
		rec := doBody(t, s, http.MethodPost, c.target, "mock-writer-token", c.body)
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("%s status = %d body=%s", c.surface, rec.Code, rec.Body.String())
		}
		var env errEnvelope
		decode(t, rec, &env)
		if env.Error.Params["surface"] != c.surface {
			t.Errorf("%s surface = %v", c.surface, env.Error.Params["surface"])
		}
	}
}
