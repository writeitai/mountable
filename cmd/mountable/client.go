package main

// The management API as the CLI commands and the MCP server use it. Results
// stay the API's JSON, so callers see the API's field names.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
)

// client calls the API with a person's login or an API key.
type client struct {
	token  string
	apiKey bool
	// org is the organisation asked for (--org); empty means resolve it.
	org string
}

// newClient authenticates with MOUNTABLE_API_KEY when it is set, otherwise
// with the login from `mountable login`.
func newClient(org string) (*client, error) {
	if key := os.Getenv("MOUNTABLE_API_KEY"); key != "" {
		return &client{token: key, apiKey: true, org: org}, nil
	}
	return loginClient(org)
}

// loginClient authenticates with the login from `mountable login`.
func loginClient(org string) (*client, error) {
	token, err := accessToken()
	if err != nil {
		return nil, &cliError{Code: "unauthenticated", Message: err.Error()}
	}
	return &client{token: token, org: org}, nil
}

// call sends one request. When the API refuses a login's access token
// before it expires (e.g. it was revoked or the API's keys rotated), call
// refreshes the login once and sends the request once more; the request is
// rebuilt, so its body is sent again in full. If the refresh fails, the
// API's refusal is the error. An API key is never retried.
func (c *client) call(ctx context.Context, method, path string, body any, out any) error {
	err := callContext(ctx, method, path, c.token, body, out)
	if c.apiKey || !unauthenticated(err) {
		return err
	}
	token, refreshErr := refreshRejected(c.token)
	if refreshErr != nil {
		return err
	}
	c.token = token
	return callContext(ctx, method, path, c.token, body, out)
}

// unauthenticated reports whether the API refused the credentials.
func unauthenticated(err error) bool {
	var api *apiError
	return errors.As(err, &api) && (api.Status == http.StatusUnauthorized || api.Code == "unauthenticated")
}

type me struct {
	Organisations []json.RawMessage `json:"organisations"`
	// APIKey is set for an API-key caller.
	APIKey *struct {
		OrgID string `json:"org_id"`
	} `json:"api_key"`
}

type orgSummary struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Role string `json:"role"`
}

// organisations lists the caller's organisations. An API key has exactly
// one: its own.
func (c *client) organisations(ctx context.Context) ([]json.RawMessage, error) {
	var m me
	if err := c.call(ctx, "GET", "/api/v1/me", nil, &m); err != nil {
		return nil, err
	}
	if !c.apiKey {
		return m.Organisations, nil
	}
	if m.APIKey == nil {
		return nil, &cliError{
			Code:    "org_required",
			Message: "the API did not report this API key's organisation",
			Hint:    "pass `--org ORG_ID` or set `MOUNTABLE_ORG`",
		}
	}
	for _, raw := range m.Organisations {
		var o orgSummary
		if json.Unmarshal(raw, &o) == nil && o.ID == m.APIKey.OrgID {
			return []json.RawMessage{raw}, nil
		}
	}
	// The key's creator may no longer be a member; the key still belongs to
	// its organisation.
	raw, err := json.Marshal(map[string]string{"id": m.APIKey.OrgID})
	return []json.RawMessage{raw}, err
}

// orgID is --org, then MOUNTABLE_ORG, then the caller's only organisation
// (an API key's own).
func (c *client) orgID(ctx context.Context) (string, error) {
	if c.org != "" {
		return c.org, nil
	}
	if org := os.Getenv("MOUNTABLE_ORG"); org != "" {
		return org, nil
	}
	orgs, err := c.organisations(ctx)
	if err != nil {
		return "", err
	}
	switch len(orgs) {
	case 1:
		var o orgSummary
		if err := json.Unmarshal(orgs[0], &o); err != nil {
			return "", err
		}
		return o.ID, nil
	case 0:
		return "", &cliError{Code: "org_required", Message: "these credentials belong to no organisation", Hint: "create one in the console at https://mountable.io"}
	default:
		return "", &cliError{Code: "org_required", Message: fmt.Sprintf("%d organisations are available", len(orgs))}
	}
}

// inOrg calls a path under the resolved organisation.
func (c *client) inOrg(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	org, err := c.orgID(ctx)
	if err != nil {
		return nil, err
	}
	var out json.RawMessage
	err = c.call(ctx, method, "/api/v1/orgs/"+url.PathEscape(org)+path, body, &out)
	return out, err
}

func (c *client) listFilesystems(ctx context.Context) (json.RawMessage, error) {
	return c.inOrg(ctx, "GET", "/filesystems", nil)
}

func (c *client) createFilesystem(ctx context.Context, name string) (json.RawMessage, error) {
	return c.inOrg(ctx, "POST", "/filesystems", map[string]string{"name": name})
}

func (c *client) filesystemUsage(ctx context.Context, fsID string) (json.RawMessage, error) {
	return c.inOrg(ctx, "GET", "/filesystems/"+url.PathEscape(fsID)+"/usage", nil)
}

func (c *client) listSessions(ctx context.Context, fsID string) (json.RawMessage, error) {
	return c.inOrg(ctx, "GET", "/filesystems/"+url.PathEscape(fsID)+"/mount-sessions", nil)
}

// revokeSession is idempotent: revoking an ended session succeeds.
func (c *client) revokeSession(ctx context.Context, sessionID string) error {
	return c.call(ctx, "DELETE", "/api/v1/mount-sessions/"+url.PathEscape(sessionID), nil, nil)
}

type sessionCreated struct {
	ID     string  `json:"id"`
	State  string  `json:"state"`
	Ticket *string `json:"ticket"`
}

// createTicket creates a mount session and returns the API's answer, which
// carries the one-time ticket. key is the idempotency key; when empty, a
// random one is generated and passed to announce before it is sent, so it
// survives a lost response. A replay of key returns the existing session
// without a ticket:
//   - still requested: its ticket was never exchanged, so its response was
//     lost. The session is revoked and replaced under a fresh key; the result
//     names the old one as replaced_session_id.
//   - active or later: the ticket was used. Nothing is revoked, and the
//     error ticket_already_used carries the existing session.
func (c *client) createTicket(ctx context.Context, fsID string, readOnly bool, key string, announce func(key string)) (json.RawMessage, error) {
	mode := "rw"
	if readOnly {
		mode = "ro"
	}
	if key == "" {
		key = randomHex(16)
		announce(key)
	}
	raw, created, err := c.postSession(ctx, fsID, mode, key)
	if err != nil {
		return nil, err
	}
	if created.Ticket != nil {
		return raw, nil
	}
	if created.State != "requested" {
		return nil, &cliError{
			Code:           "ticket_already_used",
			Message:        fmt.Sprintf("idempotency key %s already created session %s, whose ticket was used (state %s)", key, created.ID, created.State),
			IdempotencyKey: key,
			Session:        raw,
		}
	}
	if err := c.revokeSession(ctx, created.ID); err != nil {
		return nil, err
	}
	fresh := randomHex(16)
	announce(fresh)
	raw, replacement, err := c.postSession(ctx, fsID, mode, fresh)
	if err != nil {
		return nil, err
	}
	if replacement.Ticket == nil {
		return nil, &cliError{Code: "api_error", Message: "the API returned no ticket for a new idempotency key", Hint: "retry later"}
	}
	return withField(raw, "replaced_session_id", created.ID)
}

// postSession sends one create request. Every error names the key it was
// sent with. When the outcome is unknown (the request or its answer was
// lost, cut short or unreadable, or the API failed with a 5xx), a session
// may exist under that key, and the error says to retry with it.
func (c *client) postSession(ctx context.Context, fsID, mode, key string) (json.RawMessage, sessionCreated, error) {
	var raw json.RawMessage
	var created sessionCreated
	err := c.call(ctx, "POST", "/api/v1/mount-sessions", map[string]string{
		"filesystem_id": fsID, "mode": mode, "idempotency_key": key,
	}, &raw)
	if err == nil {
		err = json.Unmarshal(raw, &created)
	}
	if err == nil && created.ID == "" {
		err = errors.New("the API's answer has no session ID")
	}
	if err == nil {
		return raw, created, nil
	}
	e := toCLIError(err)
	e.IdempotencyKey = key
	var api *apiError
	if !errors.As(err, &api) || api.Status >= 500 {
		if e.Code != "network_error" {
			e.Code = "outcome_unknown"
		}
		e.Message = fmt.Sprintf("the outcome of creating a session with idempotency key %s is unknown: %s", key, e.Message)
		e.Hint = fmt.Sprintf("retry with the same idempotency key %s", key)
	}
	return nil, created, e
}

// withField adds one field to a JSON object.
func withField(raw json.RawMessage, name string, value any) (json.RawMessage, error) {
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	fields[name] = value
	return json.Marshal(fields)
}
