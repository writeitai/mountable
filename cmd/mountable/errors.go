package main

// Errors as callers see them: a stable code, a message and a next step.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
)

// cliError is what a command reports on failure. Code is the API's stable
// error code, or one of the CLI's own (org_required, ticket_already_used,
// network_error, usage, …).
type cliError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
	// RetryAfter is how many seconds to wait after rate_limited.
	RetryAfter int `json:"retry_after,omitempty"`
	// IdempotencyKey is the key a failed ticket request was sent with.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	// Session is the existing session behind ticket_already_used.
	Session json.RawMessage `json:"session,omitempty"`
}

func (e *cliError) Error() string { return e.Message }

// exitCode is 2 for wrong usage and 1 for every other error.
func (e *cliError) exitCode() int {
	if e.Code == "usage" {
		return 2
	}
	return 1
}

func usageError(usage string) *cliError {
	return &cliError{Code: "usage", Message: "usage: mountable " + usage, Hint: "run `mountable help`"}
}

// explanations gives each known code its message (when the API sends only
// the code) and its next step.
var explanations = map[string]struct{ message, hint string }{
	"unauthenticated":      {"not signed in, or the credentials were refused", "run `mountable login` or set `MOUNTABLE_API_KEY`"},
	"login_ended":          {"the login behind this request has ended", "run `mountable login` or set `MOUNTABLE_API_KEY`"},
	"payment_required":     {"the organisation needs a payment method", ""},
	"org_required":         {"several organisations are available", "pass `--org` (see `mountable orgs list`)"},
	"not_found":            {"not found, or not visible to these credentials", "check the ID with `mountable fs list`"},
	"no_grant":             {"these credentials have no grant for this filesystem in this mode", "ask an owner or admin for a grant, or use `--ro`"},
	"forbidden":            {"these credentials may not do this", "ask an owner or admin of the organisation"},
	"api_key_forbidden":    {"an API key may not do this", "use a person's login (`mountable login`) or the console"},
	"rate_limited":         {"too many requests", "wait `retry_after` seconds, then retry"},
	"idempotency_conflict": {"an identical request is in progress", "wait a moment, then retry with the same idempotency key"},
	"ticket_already_used":  {"the ticket for this idempotency key was already used", "use a new `--idempotency-key` for another mount"},
	"ticket_invalid":       {"the ticket is unknown, already used or expired", "create a new one with `mountable ticket create FS_ID`"},
	"session_not_active":   {"the mount session has ended", "create a new ticket with `mountable ticket create FS_ID`"},
	"invalid_request":      {"the API rejected the request", "check the command's arguments"},
	"network_error":        {"the API could not be reached", "check the network and MOUNTABLE_API_URL; reads and revoking sessions are safe to retry"},
}

// toCLIError turns any error into the reported form, with credentials
// removed from its message.
func toCLIError(err error) *cliError {
	var e *cliError
	var api *apiError
	var network *url.Error
	switch {
	case errors.As(err, &e):
		copied := *e
		e = &copied
	case errors.As(err, &api):
		e = &cliError{Code: api.Code, Message: api.Message, RetryAfter: api.RetryAfter}
		if e.Code == "" {
			e.Code = "api_error"
			e.Message = fmt.Sprintf("the API answered HTTP %d", api.Status)
			e.Hint = "retry later"
		}
		if e.Code == "payment_required" {
			e.Hint = api.URL
		}
	case errors.As(err, &network):
		e = &cliError{Code: "network_error", Message: err.Error()}
	default:
		e = &cliError{Code: "error", Message: err.Error()}
	}
	if known, ok := explanations[e.Code]; ok {
		if e.Message == "" {
			e.Message = known.message
		}
		if e.Hint == "" {
			e.Hint = known.hint
		}
	}
	if e.Message == "" {
		e.Message = e.Code
	}
	e.Message = redact(e.Message)
	e.Hint = redact(e.Hint)
	return e
}

// Every secret the API issues starts with "mtbl": API keys (mtbl_), login
// tokens (mtblat_, mtblrt_), tickets (mtbltk_) and invitations (mtblinv_).
var secretPattern = regexp.MustCompile(`(mtbl[a-z]*)_[A-Za-z0-9_-]+`)

// redact removes anything shaped like a Mountable secret from s.
func redact(s string) string {
	return secretPattern.ReplaceAllString(s, "${1}_[redacted]")
}
