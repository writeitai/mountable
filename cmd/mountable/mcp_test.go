package main

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func connectMCP(t *testing.T) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	serverSide, clientSide := mcp.NewInMemoryTransports()
	server, err := newMCPServer().Connect(ctx, serverSide, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, clientSide, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

// callTool returns the tool's text result and whether it is an error.
func callTool(t *testing.T, s *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("%s: %d content blocks", name, len(res.Content))
	}
	text := res.Content[0].(*mcp.TextContent).Text
	for _, secret := range []string{testKey, testLogin, testJWT, "eyJzdWIi"} {
		if strings.Contains(text, secret) {
			t.Fatalf("%s leaked a credential: %s", name, text)
		}
	}
	return text, res.IsError
}

func TestMCPTools(t *testing.T) {
	useAPIKey(t)
	s := connectMCP(t)
	var names []string
	for tool, err := range s.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, tool.Name)
		if tool.Description == "" {
			t.Errorf("%s has no description", tool.Name)
		}
	}
	slices.Sort(names)
	want := []string{"create_filesystem", "create_mount_ticket", "filesystem_usage", "list_filesystems", "list_mount_sessions", "list_organizations", "revoke_mount_session"}
	if !slices.Equal(names, want) {
		t.Fatalf("tools = %v", names)
	}
}

func TestMCPToolsReturnTheAPIsJSON(t *testing.T) {
	useAPIKey(t)
	keyMe := strings.Replace(twoOrgs, `"api_key":null`, `"api_key":{"id":"k","org_id":"`+orgA+`"}`, 1)
	usage := `{"retained_bytes":1,"byte_quota":2,"entries":3,"entry_quota":4,"bytes_read_30d":5,"bytes_written_30d":6}`
	sessions := `[{"id":"s1","state":"active"}]`
	created := `{"id":"fsnew","name":"x"}`
	api := newFakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/v1/me": reply(200, keyMe),
		"GET /api/v1/orgs/" + orgA + "/filesystems":                      reply(200, filesystems),
		"POST /api/v1/orgs/" + orgB + "/filesystems":                     reply(201, created),
		"GET /api/v1/orgs/" + orgA + "/filesystems/fsone/usage":          reply(200, usage),
		"GET /api/v1/orgs/" + orgA + "/filesystems/fsone/mount-sessions": reply(200, sessions),
		"DELETE /api/v1/mount-sessions/s1":                               reply(204, ""),
	})
	s := connectMCP(t)
	cases := []struct {
		tool string
		args map[string]any
		want string
	}{
		{"list_organizations", nil, `[{"id":"` + orgA + `","name":"Alpha","role":"owner"}]`},
		{"list_filesystems", nil, filesystems},
		{"create_filesystem", map[string]any{"name": "x", "org_id": orgB}, created},
		{"filesystem_usage", map[string]any{"filesystem_id": "fsone"}, usage},
		{"list_mount_sessions", map[string]any{"filesystem_id": "fsone"}, sessions},
		{"revoke_mount_session", map[string]any{"session_id": "s1"}, `{"id":"s1","revoked":true}`},
	}
	for _, tc := range cases {
		got, isError := callTool(t, s, tc.tool, tc.args)
		if isError || got != tc.want {
			t.Errorf("%s = %v %s, want %s (calls %v)", tc.tool, isError, got, tc.want, api.called())
		}
	}
}

func TestMCPCreateMountTicket(t *testing.T) {
	useAPIKey(t)
	api := newFakeAPI(t, map[string]http.HandlerFunc{
		"POST /api/v1/mount-sessions": reply(201, session("s1", "requested", "mtbltk_theticket")),
	})
	s := connectMCP(t)
	text, isError := callTool(t, s, "create_mount_ticket", map[string]any{"filesystem_id": "fsone", "read_only": true, "idempotency_key": "job-1"})
	var out map[string]any
	if isError || json.Unmarshal([]byte(text), &out) != nil {
		t.Fatalf("result = %v %s", isError, text)
	}
	command, _ := out["mount_command"].(string)
	if out["ticket"] != "mtbltk_theticket" || !strings.HasPrefix(command, "mkdir -p /mnt/mountable && ") ||
		!strings.Contains(command, "| mountable mount --ticket-stdin") || strings.Contains(command, "mtbltk_") {
		t.Fatalf("result = %s", text)
	}
	if b := api.bodies[0]; b["mode"] != "ro" || b["idempotency_key"] != "job-1" {
		t.Fatalf("body = %v", b)
	}
}

func TestMCPTicketReplayFollowsTheCLIRules(t *testing.T) {
	useAPIKey(t)
	newFakeAPI(t, map[string]http.HandlerFunc{
		"POST /api/v1/mount-sessions": reply(201, session("s1", "active", "")),
	})
	s := connectMCP(t)
	text, isError := callTool(t, s, "create_mount_ticket", map[string]any{"filesystem_id": "fsone", "idempotency_key": "job-1"})
	var out struct {
		Error cliError `json:"error"`
	}
	if !isError || json.Unmarshal([]byte(text), &out) != nil || out.Error.Code != "ticket_already_used" || out.Error.Hint == "" {
		t.Fatalf("result = %v %s", isError, text)
	}
}

func TestMCPErrorsCarryCodeAndHint(t *testing.T) {
	useAPIKey(t)
	newFakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/v1/orgs/" + orgA + "/filesystems/nope/usage": reply(404, `{"detail":{"code":"not_found"}}`),
	})
	t.Setenv("MOUNTABLE_ORG", orgA)
	s := connectMCP(t)
	text, isError := callTool(t, s, "filesystem_usage", map[string]any{"filesystem_id": "nope"})
	var out struct {
		Error cliError `json:"error"`
	}
	if !isError || json.Unmarshal([]byte(text), &out) != nil || out.Error.Code != "not_found" || !strings.Contains(out.Error.Hint, "fs list") {
		t.Fatalf("result = %v %s", isError, text)
	}

	t.Setenv("MOUNTABLE_API_KEY", "")
	text, isError = callTool(t, s, "list_filesystems", nil)
	if !isError || !strings.Contains(text, `"unauthenticated"`) {
		t.Fatalf("without credentials: %v %s", isError, text)
	}
}

// MCP errors are redacted in every field, including the session.
func TestMCPErrorFieldsAreRedacted(t *testing.T) {
	useAPIKey(t)
	newFakeAPI(t, map[string]http.HandlerFunc{
		"POST /api/v1/mount-sessions": reply(201, escapedSession),
	})
	s := connectMCP(t)
	text, isError := callTool(t, s, "create_mount_ticket", map[string]any{"filesystem_id": "fsone", "idempotency_key": "mtbl_keysecret"})
	var out struct {
		Error cliError `json:"error"`
	}
	if !isError || json.Unmarshal([]byte(text), &out) != nil || out.Error.Code != "ticket_already_used" ||
		out.Error.IdempotencyKey != "mtbl_[redacted]" || strings.Contains(text, "keysecret") || leaksEscapedSecret(text) ||
		!strings.Contains(string(out.Error.Session), `"created_by":"mtblat_[redacted]"`) {
		t.Fatalf("result = %v %s", isError, text)
	}
}

// A replacement whose 201 answer is cut short leaves the caller the fresh
// key, and retrying with it recovers a working ticket.
func TestMCPLostReplacementCanBeRecovered(t *testing.T) {
	useAPIKey(t)
	var mu sync.Mutex
	lost := map[string]bool{} // keys whose session was created but whose answer was lost
	posts := 0
	api := newFakeAPI(t, map[string]http.HandlerFunc{
		"POST /api/v1/mount-sessions": func(w http.ResponseWriter, r *http.Request) {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			key := body["idempotency_key"]
			mu.Lock()
			defer mu.Unlock()
			posts++
			switch {
			case key == "job-1" || lost[key]:
				reply(201, session("s-"+key, "requested", ""))(w, r)
			case posts == 2:
				lost[key] = true
				cutShort(w, r)
			default:
				reply(201, session("s-"+key, "requested", "mtbltk_recovered"))(w, r)
			}
		},
		"DELETE /api/v1/mount-sessions/s-job-1": reply(204, ""),
	})
	s := connectMCP(t)
	text, isError := callTool(t, s, "create_mount_ticket", map[string]any{"filesystem_id": "fsone", "idempotency_key": "job-1"})
	var out struct {
		Error cliError `json:"error"`
	}
	if !isError || json.Unmarshal([]byte(text), &out) != nil || out.Error.Code != "outcome_unknown" || out.Error.IdempotencyKey == "" {
		t.Fatalf("lost replacement: %v %s", isError, text)
	}
	key := out.Error.IdempotencyKey
	mu.Lock()
	created := lost[key]
	mu.Unlock()
	if !created || !strings.Contains(out.Error.Hint, key) {
		t.Fatalf("the error names %q, not the replacement's key", key)
	}

	// Retrying with that key finds the unused replacement and replaces it.
	api.routes["DELETE /api/v1/mount-sessions/s-"+key] = reply(204, "")
	text, isError = callTool(t, s, "create_mount_ticket", map[string]any{"filesystem_id": "fsone", "idempotency_key": key})
	var ok map[string]any
	if isError || json.Unmarshal([]byte(text), &ok) != nil || ok["ticket"] != "mtbltk_recovered" || ok["replaced_session_id"] != "s-"+key {
		t.Fatalf("retry: %v %s", isError, text)
	}
}
