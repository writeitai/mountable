package main

// `mountable mcp`: the management commands as MCP tools over stdio.
// Mounting stays a local process: create_mount_ticket returns the command
// that mounts in the sandbox.

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	retryRead  = " Read-only and safe to retry."
	rateLimits = " On rate_limited, wait retry_after seconds before retrying."
	orgNote    = " org_id is optional: an API key uses its own organisation, and a login with one organisation uses that one."
)

// mountCommandTemplate creates the mount directory, then mounts with the
// ticket from MOUNTABLE_TICKET, read on stdin so it never appears in an
// argument list.
const mountCommandTemplate = `mkdir -p /mnt/mountable && printf '%s\n' "$MOUNTABLE_TICKET" | mountable mount --ticket-stdin --json /mnt/mountable`

type orgInput struct {
	OrgID string `json:"org_id,omitempty" jsonschema:"the organisation ID; optional"`
}

type createFilesystemInput struct {
	Name  string `json:"name" jsonschema:"the filesystem's name; names need not be unique"`
	OrgID string `json:"org_id,omitempty" jsonschema:"the organisation ID; optional"`
}

type filesystemInput struct {
	FilesystemID string `json:"filesystem_id" jsonschema:"the filesystem ID"`
	OrgID        string `json:"org_id,omitempty" jsonschema:"the organisation ID; optional"`
}

type ticketInput struct {
	FilesystemID   string `json:"filesystem_id" jsonschema:"the filesystem ID"`
	ReadOnly       bool   `json:"read_only,omitempty" jsonschema:"mount read-only"`
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"a key you choose and reuse when retrying this request; a new key for each new mount"`
}

type sessionInput struct {
	SessionID string `json:"session_id" jsonschema:"the mount session ID"`
}

func serveMCP() error {
	return newMCPServer().Run(context.Background(), &mcp.StdioTransport{})
}

func newMCPServer() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "mountable", Version: version}, nil)
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_organizations",
		Description: "List the caller's Mountable organisations (an API key has exactly one)." + retryRead + rateLimits,
	}, tool(func(ctx context.Context, c *client, _ struct{}) (any, error) {
		return c.organisations(ctx)
	}))
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_filesystems",
		Description: "List the organisation's filesystems (an API key sees those it has a grant on)." + orgNote + retryRead + rateLimits,
	}, toolIn(func(in orgInput) string { return in.OrgID }, func(ctx context.Context, c *client, _ orgInput) (any, error) {
		return c.listFilesystems(ctx)
	}))
	mcp.AddTool(s, &mcp.Tool{
		Name: "create_filesystem",
		Description: "Create a filesystem and return it; its id is what tickets and mounts need." + orgNote +
			" Not idempotent, and names need not be unique: after a timeout or an unknown outcome, call list_filesystems and look for the name before creating again. Never retry blindly." + rateLimits,
	}, toolIn(func(in createFilesystemInput) string { return in.OrgID }, func(ctx context.Context, c *client, in createFilesystemInput) (any, error) {
		return c.createFilesystem(ctx, in.Name)
	}))
	mcp.AddTool(s, &mcp.Tool{
		Name:        "filesystem_usage",
		Description: "A filesystem's stored bytes and files against its quotas, and its traffic over 30 days." + orgNote + retryRead + rateLimits,
	}, toolIn(func(in filesystemInput) string { return in.OrgID }, func(ctx context.Context, c *client, in filesystemInput) (any, error) {
		return c.filesystemUsage(ctx, in.FilesystemID)
	}))
	mcp.AddTool(s, &mcp.Tool{
		Name: "create_mount_ticket",
		Description: "Create a one-time mount ticket for a sandbox. The result is the mount session with its ticket (single use, valid for minutes) and mount_command, the command to run in the sandbox with the ticket in the environment variable MOUNTABLE_TICKET; it reads the ticket on stdin, so never put the ticket in an argument list. /mnt/mountable is only a default: any writable directory works. The sandbox needs the mountable CLI (curl -fsSL https://mountable.io/install.sh | sh) and FUSE; wait for the JSON line {\"event\":\"mounted\"} before using the directory." +
			" Retries: pass your own idempotency_key and reuse it when retrying after an unknown outcome. A replay returns the existing session without a ticket. If that session is still requested, its ticket was lost: this tool revokes it and creates a new one with a fresh key, and the result names the old one as replaced_session_id. If it is active or later, the ticket was used: nothing is revoked and the tool fails with ticket_already_used; use a new idempotency_key for another mount. idempotency_conflict means an identical request is in progress. If the outcome is unknown (network_error or outcome_unknown), the error's idempotency_key is the key that request was sent with: retry with exactly that key, even if you did not choose it." + rateLimits,
	}, tool(func(ctx context.Context, c *client, in ticketInput) (any, error) {
		raw, err := c.createTicket(ctx, in.FilesystemID, in.ReadOnly, in.IdempotencyKey, func(string) {})
		if err != nil {
			return nil, err
		}
		return withField(raw, "mount_command", mountCommandTemplate)
	}))
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_mount_sessions",
		Description: "List a filesystem's live mount sessions (an API key sees those it created)." + orgNote + retryRead + rateLimits,
	}, toolIn(func(in filesystemInput) string { return in.OrgID }, func(ctx context.Context, c *client, in filesystemInput) (any, error) {
		return c.listSessions(ctx, in.FilesystemID)
	}))
	mcp.AddTool(s, &mcp.Tool{
		Name:        "revoke_mount_session",
		Description: "Revoke a mount session; its mount stops working. Idempotent and safe to retry." + rateLimits,
	}, tool(func(ctx context.Context, c *client, in sessionInput) (any, error) {
		if err := c.revokeSession(ctx, in.SessionID); err != nil {
			return nil, err
		}
		return map[string]any{"id": in.SessionID, "revoked": true}, nil
	}))
	return s
}

// tool adapts fn, which uses the organisation it is given or resolves one,
// to an MCP tool handler: results are the API's JSON, and errors carry the
// code and hint.
func tool[In any](fn func(ctx context.Context, c *client, in In) (any, error)) mcp.ToolHandlerFor[In, any] {
	return toolIn(func(In) string { return "" }, fn)
}

func toolIn[In any](org func(In) string, fn func(ctx context.Context, c *client, in In) (any, error)) mcp.ToolHandlerFor[In, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		c, err := newClient(org(in))
		var result any
		if err == nil {
			result, err = fn(ctx, c, in)
		}
		if err != nil {
			data, _ := json.Marshal(map[string]any{"error": toCLIError(err)})
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}, nil, nil
		}
		data, err := json.Marshal(result)
		if err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(data)}}}, nil, nil
	}
}
