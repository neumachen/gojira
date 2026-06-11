package client

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// APIError is a structured representation of a Jira REST API error
// response, returned for 400 (Bad Request) and 409 (Conflict) statuses.
// It wraps the matching sentinel ([ErrBadRequest] or [ErrConflict]) so
// callers can both classify the failure with [errors.Is] and inspect
// which specific fields the server rejected via [errors.As].
//
// Jira Cloud v3 returns errors in the shape
//
//	{"errorMessages": ["..."], "errors": {"summary": "...", "...": "..."}}
//
// Either key may be absent. When the body is empty, not JSON, or shaped
// differently, [parseAPIError] still returns an *APIError carrying only
// the status and the sentinel — degrading gracefully so classification
// is never lost.
type APIError struct {
	// Status is the HTTP status code that produced this error (400 or 409
	// for Jira REST v3 in the current client).
	Status int

	// Messages are the top-level "errorMessages" entries from the Jira
	// response body. nil/empty when the body had none.
	Messages []string

	// FieldErrors maps a Jira field id (e.g. "summary",
	// "customfield_10010") to its per-field validation message. nil/empty
	// when the body had no "errors" object.
	FieldErrors map[string]string

	// sentinel is the wrapped sentinel error (ErrBadRequest or
	// ErrConflict) that errors.Is targets via Unwrap. It is unexported
	// because callers never set it directly — they construct *APIError
	// through parseAPIError, which always supplies the right sentinel.
	sentinel error
}

// Error implements the error interface. The returned string is stable
// for a given *APIError value: field-error keys are sorted
// alphabetically so message text does not depend on Go's map iteration
// order, which keeps log lines and test assertions reproducible.
//
// Shape (omitted sections are dropped):
//
//	"client: bad request (400): <message1>; <message2> [field1=msg; field2=msg]"
func (e *APIError) Error() string {
	if e == nil {
		return "<nil *APIError>"
	}

	var b strings.Builder

	// Prefer the sentinel's own text (which already includes the status
	// word, e.g. "client: bad request (400)") so the surface stays
	// consistent with the bare-sentinel form. Fall back to a status-only
	// preamble when no sentinel was attached, so we still produce
	// something useful.
	if e.sentinel != nil {
		b.WriteString(e.sentinel.Error())
	} else {
		fmt.Fprintf(&b, "client: api error (%d)", e.Status)
	}

	if len(e.Messages) > 0 {
		b.WriteString(": ")
		b.WriteString(strings.Join(e.Messages, "; "))
	}

	if len(e.FieldErrors) > 0 {
		keys := make([]string, 0, len(e.FieldErrors))
		for k := range e.FieldErrors {
			keys = append(keys, k)
		}
		sort.Strings(keys)

		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+e.FieldErrors[k])
		}
		b.WriteString(" [")
		b.WriteString(strings.Join(parts, "; "))
		b.WriteString("]")
	}

	return b.String()
}

// Unwrap returns the wrapped sentinel so errors.Is(err, ErrBadRequest)
// (or ErrConflict) classifies an *APIError exactly as it would the
// bare sentinel. Returning the sentinel rather than nil is what makes
// the typed error additive over the existing sentinel API.
func (e *APIError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.sentinel
}

// jiraErrorBody is the on-the-wire shape of Jira's standard error
// response. We unmarshal into this local type rather than hand-parsing
// json.RawMessage so an unparseable body falls through to the
// degraded-but-still-classifying APIError below.
type jiraErrorBody struct {
	ErrorMessages []string          `json:"errorMessages"`
	Errors        map[string]string `json:"errors"`
}

// parseAPIError builds an [*APIError] from a Jira error-response body
// for the given status and sentinel. When body is empty, not JSON, or
// not shaped like a Jira error, the returned *APIError carries only
// Status and the sentinel — Messages and FieldErrors are nil. This
// guarantees [errors.Is] classification is preserved on every 400/409,
// even when a reverse proxy / WAF interposes an HTML body in place of
// the JSON one.
//
// parseAPIError is unexported because callers reach it only through
// the doWithRetry status switch; tests exercise it indirectly via
// httptest servers in client_test.
func parseAPIError(status int, sentinel error, body []byte) *APIError {
	ape := &APIError{Status: status, sentinel: sentinel}

	if len(body) == 0 {
		return ape
	}

	var parsed jiraErrorBody
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ape
	}
	ape.Messages = parsed.ErrorMessages
	ape.FieldErrors = parsed.Errors
	return ape
}
