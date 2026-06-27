// Package apierr is the BFF error model. Every non-2xx response is rendered as
// the contract section 1 envelope: a JSON-pointer `code` into the i18n tree,
// interpolation `params`, a non-localized developer `message`, and the
// correlation `request_id`. Codes are stable identifiers and are duplicated as
// a typed const here (the UI mirrors them in TS); a test asserts this registry
// matches the contract table exactly.
package apierr

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// Code is a JSON Pointer (RFC 6901) into the i18n `errors` subtree.
type Code string

// Canonical error codes shipped in PR1 (contract section 1).
const (
	CodePageSizeOutOfRange Code = "/errors/validation/page_size_out_of_range"
	CodeInvalidCursor      Code = "/errors/validation/invalid_cursor"
	CodeInvalidAsOf        Code = "/errors/validation/invalid_as_of"
	CodeInvalidLSNRange    Code = "/errors/validation/invalid_lsn_range"
	CodeMissingToken       Code = "/errors/auth/missing_token"
	CodeInvalidToken       Code = "/errors/auth/invalid_token"
	CodeForbidden          Code = "/errors/auth/forbidden"
	CodeNotFound           Code = "/errors/not_found"
	CodeUnimplemented      Code = "/errors/upstream/unimplemented"
	CodeUnavailable        Code = "/errors/upstream/unavailable"
	CodeInternal           Code = "/errors/internal"
)

// Descriptor is the registry entry for a code: its HTTP status and the set of
// param keys the localized template interpolates.
type Descriptor struct {
	HTTPStatus int
	ParamKeys  []string
}

// Registry is the authoritative mapping of every PR1 code to its HTTP status
// and declared params. It mirrors the contract section 1 table.
var Registry = map[Code]Descriptor{
	CodePageSizeOutOfRange: {HTTPStatus: http.StatusBadRequest, ParamKeys: []string{"min", "max", "got"}},
	CodeInvalidCursor:      {HTTPStatus: http.StatusBadRequest, ParamKeys: []string{"cursor"}},
	CodeInvalidAsOf:        {HTTPStatus: http.StatusBadRequest, ParamKeys: []string{"value"}},
	CodeInvalidLSNRange:    {HTTPStatus: http.StatusBadRequest, ParamKeys: []string{"from", "to"}},
	CodeMissingToken:       {HTTPStatus: http.StatusUnauthorized, ParamKeys: []string{}},
	CodeInvalidToken:       {HTTPStatus: http.StatusUnauthorized, ParamKeys: []string{}},
	CodeForbidden:          {HTTPStatus: http.StatusForbidden, ParamKeys: []string{}},
	CodeNotFound:           {HTTPStatus: http.StatusNotFound, ParamKeys: []string{"resource"}},
	CodeUnimplemented:      {HTTPStatus: http.StatusNotImplemented, ParamKeys: []string{"surface"}},
	CodeUnavailable:        {HTTPStatus: http.StatusBadGateway, ParamKeys: []string{"surface"}},
	CodeInternal:           {HTTPStatus: http.StatusInternalServerError, ParamKeys: []string{}},
}

// APIError is a renderable, typed error. It implements the error interface so
// it can travel through normal Go control flow and be matched at the edge.
type APIError struct {
	Code    Code
	Status  int
	Params  map[string]any
	Message string
}

// Error implements the error interface with the developer-facing message.
func (e *APIError) Error() string { return string(e.Code) + ": " + e.Message }

// New builds an APIError for a registered code. The HTTP status is taken from
// the registry, keeping status and code consistent by construction.
func New(code Code, message string, params map[string]any) *APIError {
	d, ok := Registry[code]
	status := http.StatusInternalServerError
	if ok {
		status = d.HTTPStatus
	}
	if params == nil {
		params = map[string]any{}
	}
	return &APIError{Code: code, Status: status, Params: params, Message: message}
}

// Typed constructors for each PR1 code keep params well-formed at call sites.

// PageSizeOutOfRange reports a page_size outside [min,max].
func PageSizeOutOfRange(min, max, got int) *APIError {
	return New(CodePageSizeOutOfRange,
		fmt.Sprintf("page_size %d is out of range [%d,%d]", got, min, max),
		map[string]any{"min": min, "max": max, "got": got})
}

// InvalidCursor reports an undecodable or malformed pagination cursor.
func InvalidCursor(cursor string) *APIError {
	return New(CodeInvalidCursor, fmt.Sprintf("invalid cursor %q", cursor),
		map[string]any{"cursor": cursor})
}

// InvalidAsOf reports a malformed as_of timestamp.
func InvalidAsOf(value string) *APIError {
	return New(CodeInvalidAsOf, fmt.Sprintf("invalid as_of %q (want RFC3339 or YYYY-MM-DD)", value),
		map[string]any{"value": value})
}

// InvalidLSNRange reports an lsn window whose lower bound exceeds its upper
// bound (from_lsn > to_lsn).
func InvalidLSNRange(from, to int64) *APIError {
	return New(CodeInvalidLSNRange,
		fmt.Sprintf("invalid lsn range: from_lsn %d is greater than to_lsn %d", from, to),
		map[string]any{"from": from, "to": to})
}

// MissingToken reports an absent Authorization header.
func MissingToken() *APIError {
	return New(CodeMissingToken, "missing bearer token", nil)
}

// InvalidToken reports an unrecognized bearer token.
func InvalidToken() *APIError {
	return New(CodeInvalidToken, "invalid bearer token", nil)
}

// Forbidden reports an authenticated principal lacking the required role.
func Forbidden() *APIError {
	return New(CodeForbidden, "forbidden", nil)
}

// NotFound reports a missing resource.
func NotFound(resource string) *APIError {
	return New(CodeNotFound, fmt.Sprintf("resource %q not found", resource),
		map[string]any{"resource": resource})
}

// Unimplemented reports an upstream surface Core has not yet implemented.
func Unimplemented(surface string) *APIError {
	return New(CodeUnimplemented, fmt.Sprintf("upstream surface %q is unimplemented", surface),
		map[string]any{"surface": surface})
}

// Unavailable reports an upstream surface that could not be reached.
func Unavailable(surface string) *APIError {
	return New(CodeUnavailable, fmt.Sprintf("upstream surface %q is unavailable", surface),
		map[string]any{"surface": surface})
}

// Internal reports an unexpected server-side failure.
func Internal(message string) *APIError {
	if message == "" {
		message = "internal server error"
	}
	return New(CodeInternal, message, nil)
}

// envelope is the wire shape of an error response.
type envelope struct {
	Error body `json:"error"`
}

type body struct {
	Code      Code           `json:"code"`
	Params    map[string]any `json:"params"`
	Message   string         `json:"message"`
	RequestID string         `json:"request_id"`
}

// Write renders err as the contract error envelope. A nil or non-APIError is
// coerced to a generic 500 so the edge never leaks an untyped error.
func Write(w http.ResponseWriter, requestID string, err error) {
	ae, ok := err.(*APIError)
	if !ok || ae == nil {
		msg := "internal server error"
		if err != nil {
			msg = err.Error()
		}
		ae = Internal(msg)
	}
	params := ae.Params
	if params == nil {
		params = map[string]any{}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(ae.Status)
	_ = json.NewEncoder(w).Encode(envelope{Error: body{
		Code:      ae.Code,
		Params:    params,
		Message:   ae.Message,
		RequestID: requestID,
	}})
}
