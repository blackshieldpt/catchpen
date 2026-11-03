// Package api is the wire contract of the catchpen API that `catchpen daemon`
// serves: the API version, error codes, the paths and types of its
// endpoints, stream frames and HMAC request signing. The server and its
// clients (pkg/apiclient, the cockpit) both use it, so the two can't drift.
// It imports only the standard library.
//
// Text that came from a VM's guest (task output, egress hosts, audit details,
// terminal bytes) is passed on as data: a client must make it safe before
// showing it.
package api

import (
	"fmt"
	"net/http"
)

// Version is the API version. A change a client could notice in a way that
// breaks it gets a new version, under a new path prefix.
const Version = 1

// Prefix is the path every route of this version starts with.
const Prefix = "/v1"

// MaxRequestBody is the largest request body a server accepts. The HMAC
// signature covers the whole body, so it is read in full before it is
// checked.
const MaxRequestBody = 4 << 20

// ContentTypeNDJSON is the type of a follow response: one JSON value per
// line, until the response ends. A stream that fails after it started ends
// with a line holding an ErrorResponse.
const ContentTypeNDJSON = "application/x-ndjson"

// Error codes, in Error.Code.
const (
	CodeBadRequest      = "bad_request"
	CodeUnauthorized    = "unauthorized" // no valid client credentials
	CodeForbidden       = "forbidden"    // the client lacks a scope
	CodeNotFound        = "not_found"
	CodeConflict        = "conflict"         // the state changed, or doesn't allow it
	CodeVMPaused        = "vm_paused"        // a canary hit paused the VM
	CodeConfirmRequired = "confirm_required" // a destructive call without confirm
	CodeTooLarge        = "too_large"        // over MaxRequestBody
	CodeRateLimited     = "rate_limited"
	CodeInternal        = "internal"
)

// Status is the HTTP status a server answers with for code; an unknown code
// is a server error.
func Status(code string) int {
	switch code {
	case CodeBadRequest:
		return http.StatusBadRequest
	case CodeUnauthorized:
		return http.StatusUnauthorized
	case CodeForbidden:
		return http.StatusForbidden
	case CodeNotFound:
		return http.StatusNotFound
	case CodeConflict, CodeVMPaused:
		return http.StatusConflict
	case CodeConfirmRequired:
		return http.StatusPreconditionRequired
	case CodeTooLarge:
		return http.StatusRequestEntityTooLarge
	case CodeRateLimited:
		return http.StatusTooManyRequests
	}
	return http.StatusInternalServerError
}

// Error is an API error: a code from the list above and a message for people.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// ErrorResponse is the body of a response that failed.
type ErrorResponse struct {
	Error *Error `json:"error"`
}

// List returns s, or an empty slice when s is nil, so a list encodes as []
// and never as null.
func List[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
