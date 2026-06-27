package apierr

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// contractRow is one parsed row of the contract section 1 error table.
type contractRow struct {
	status    int
	code      string
	paramKeys []string
}

// parseContractTable extracts the canonical error table from the committed
// api-contract.md so the registry test asserts against the source of truth.
func parseContractTable(t *testing.T) map[string]contractRow {
	t.Helper()
	path := filepath.Join("..", "..", "docs", "api-contract.md")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open contract: %v", err)
	}
	defer f.Close()

	rows := map[string]contractRow{}
	sc := bufio.NewScanner(f)
	inTable := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		// The table header in section 1 is exactly "| HTTP | code | params |".
		if strings.HasPrefix(line, "| HTTP | code | params |") {
			inTable = true
			continue
		}
		if !inTable {
			continue
		}
		if !strings.HasPrefix(line, "|") {
			break // table ended
		}
		if strings.HasPrefix(line, "|---") {
			continue // separator row
		}
		cells := splitRow(line)
		if len(cells) != 3 {
			continue
		}
		status, err := strconv.Atoi(strings.TrimSpace(cells[0]))
		if err != nil {
			continue
		}
		code := strings.Trim(strings.TrimSpace(cells[1]), "`")
		rows[code] = contractRow{
			status:    status,
			code:      code,
			paramKeys: parseParams(cells[2]),
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan contract: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("parsed zero contract rows; table format changed?")
	}
	return rows
}

func splitRow(line string) []string {
	parts := strings.Split(line, "|")
	// Drop the leading and trailing empty segments around the outer pipes.
	if len(parts) >= 2 {
		parts = parts[1 : len(parts)-1]
	}
	return parts
}

func parseParams(cell string) []string {
	cell = strings.TrimSpace(cell)
	cell = strings.Trim(cell, "`")
	if cell == "" || cell == "{}" {
		return []string{}
	}
	var out []string
	for _, p := range strings.Split(cell, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func TestRegistryMatchesContractTable(t *testing.T) {
	contract := parseContractTable(t)

	if len(contract) != len(Registry) {
		t.Fatalf("row count mismatch: contract has %d, registry has %d", len(contract), len(Registry))
	}

	for code, row := range contract {
		desc, ok := Registry[Code(code)]
		if !ok {
			t.Errorf("contract code %q missing from Registry", code)
			continue
		}
		if desc.HTTPStatus != row.status {
			t.Errorf("code %q: status mismatch contract=%d registry=%d", code, row.status, desc.HTTPStatus)
		}
		got := append([]string{}, desc.ParamKeys...)
		want := append([]string{}, row.paramKeys...)
		sort.Strings(got)
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("code %q: param keys mismatch contract=%v registry=%v", code, want, got)
		}
	}

	// And the reverse: every registry code must exist in the contract.
	for code := range Registry {
		if _, ok := contract[string(code)]; !ok {
			t.Errorf("registry code %q not present in contract table", code)
		}
	}
}

func TestWriteEnvelope(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantCode   Code
		wantParams map[string]any
	}{
		{
			name:       "validation with params",
			err:        PageSizeOutOfRange(1, 200, 999),
			wantStatus: http.StatusBadRequest,
			wantCode:   CodePageSizeOutOfRange,
			wantParams: map[string]any{"min": float64(1), "max": float64(200), "got": float64(999)},
		},
		{
			name:       "auth empty params rendered as object",
			err:        MissingToken(),
			wantStatus: http.StatusUnauthorized,
			wantCode:   CodeMissingToken,
			wantParams: map[string]any{},
		},
		{
			name:       "unimplemented surface",
			err:        Unimplemented("anchors"),
			wantStatus: http.StatusNotImplemented,
			wantCode:   CodeUnimplemented,
			wantParams: map[string]any{"surface": "anchors"},
		},
		{
			name:       "non-apierror coerced to 500",
			err:        errStr("boom"),
			wantStatus: http.StatusInternalServerError,
			wantCode:   CodeInternal,
			wantParams: map[string]any{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			Write(rec, "req_test123", tc.err)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Errorf("content-type = %q", ct)
			}

			var env envelope
			if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode: %v (%s)", err, rec.Body.String())
			}
			if env.Error.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", env.Error.Code, tc.wantCode)
			}
			if env.Error.RequestID != "req_test123" {
				t.Errorf("request_id = %q", env.Error.RequestID)
			}
			if env.Error.Message == "" {
				t.Error("message must be non-empty (dev fallback)")
			}
			if env.Error.Params == nil {
				t.Error("params must never be null; want {} at minimum")
			}
			if !reflect.DeepEqual(env.Error.Params, tc.wantParams) {
				t.Errorf("params = %#v, want %#v", env.Error.Params, tc.wantParams)
			}
		})
	}
}

type errStr string

func (e errStr) Error() string { return string(e) }
