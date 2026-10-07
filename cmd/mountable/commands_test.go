package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

const (
	testKey   = "mtbl_testkeysecret"
	testLogin = "mtblat_testloginsecret"
	orgA      = "11111111-1111-1111-1111-111111111111"
	orgB      = "22222222-2222-2222-2222-222222222222"
)

// syncBuffer is a buffer the fake API may read while a command writes it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fakeAPI answers "METHOD /path" routes and records each request.
type fakeAPI struct {
	t      *testing.T
	routes map[string]http.HandlerFunc
	mu     sync.Mutex
	calls  []string
	bodies []map[string]string
	auth   []string
	url    string
}

func newFakeAPI(t *testing.T, routes map[string]http.HandlerFunc) *fakeAPI {
	f := &fakeAPI{t: t, routes: routes}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := r.Method + " " + r.URL.Path
		var body map[string]string
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
		r.Body = io.NopCloser(bytes.NewReader(data))
		f.mu.Lock()
		f.calls = append(f.calls, route)
		f.bodies = append(f.bodies, body)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.mu.Unlock()
		if h, ok := f.routes[route]; ok {
			h(w, r)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"detail":{"code":"not_found"}}`)
	}))
	t.Cleanup(srv.Close)
	f.url = srv.URL
	t.Setenv("MOUNTABLE_API_URL", srv.URL)
	t.Setenv("MOUNTABLE_ORG", "")
	return f
}

func (f *fakeAPI) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func reply(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}
}

func useAPIKey(t *testing.T) {
	t.Setenv("MOUNTABLE_API_KEY", testKey)
	isolateConfig(t)
}

func useLogin(t *testing.T) {
	t.Setenv("MOUNTABLE_API_KEY", "")
	isolateConfig(t)
	if err := saveCredentials(tokenResponse{AccessToken: testLogin, RefreshToken: "mtblrt_refreshsecret", ExpiresIn: 3600}); err != nil {
		t.Fatal(err)
	}
}

func isolateConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

type result struct {
	stdout, stderr string
	code           int
}

// runCLI runs one command and checks that no credential leaked into its
// output.
func runCLI(t *testing.T, args ...string) result {
	t.Helper()
	var stdout, stderr syncBuffer
	return runWith(t, &stdout, &stderr, args...)
}

func runWith(t *testing.T, stdout, stderr *syncBuffer, args ...string) result {
	t.Helper()
	code := run(args, stdout, stderr)
	r := result{stdout: stdout.String(), stderr: stderr.String(), code: code}
	for _, secret := range []string{testKey, testLogin, "mtblrt_refreshsecret"} {
		if strings.Contains(r.stdout+r.stderr, secret) {
			t.Fatalf("%v leaked a credential:\nstdout: %s\nstderr: %s", args, r.stdout, r.stderr)
		}
	}
	return r
}

// jsonError decodes {"error": …} from stdout.
func jsonError(t *testing.T, r result) cliError {
	t.Helper()
	var out struct {
		Error *cliError `json:"error"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil || out.Error == nil {
		t.Fatalf("stdout is not one JSON error: %q (%v)", r.stdout, err)
	}
	return *out.Error
}

const twoOrgs = `{"id":"m","email":"a@example.com","name":"A","organisations":[` +
	`{"id":"` + orgA + `","name":"Alpha","role":"owner"},{"id":"` + orgB + `","name":"Beta","role":"member"}],"api_key":null}`

const oneOrg = `{"id":"m","email":"a@example.com","name":"A","organisations":[{"id":"` + orgA + `","name":"Alpha","role":"owner"}],"api_key":null}`

const filesystems = `[{"id":"fsone","name":"data","byte_quota":10,"entry_quota":5,"created_at":"2026-10-01T00:00:00Z"}]`

func TestOrgsList(t *testing.T) {
	api := newFakeAPI(t, map[string]http.HandlerFunc{"GET /api/v1/me": reply(200, twoOrgs)})
	useLogin(t)

	r := runCLI(t, "orgs", "list", "--json")
	var orgs []map[string]any
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &orgs) != nil || len(orgs) != 2 || orgs[1]["name"] != "Beta" {
		t.Fatalf("orgs list --json = %d %q", r.code, r.stdout)
	}
	if r.stderr != "" {
		t.Errorf("stderr = %q", r.stderr)
	}
	r = runCLI(t, "orgs", "list")
	if r.code != 0 || !strings.Contains(r.stdout, "Alpha") || !strings.Contains(r.stdout, orgB) {
		t.Fatalf("orgs list = %d %q", r.code, r.stdout)
	}
	if api.auth[0] != "Bearer "+testLogin {
		t.Errorf("authorization = %q", api.auth[0])
	}
}

func TestOrgsListWithAPIKeyShowsTheKeysOrganisation(t *testing.T) {
	useAPIKey(t)
	me := strings.Replace(twoOrgs, `"api_key":null`, `"api_key":{"id":"k","org_id":"`+orgB+`"}`, 1)
	api := newFakeAPI(t, map[string]http.HandlerFunc{"GET /api/v1/me": reply(200, me)})

	r := runCLI(t, "orgs", "list", "--json")
	var orgs []orgSummary
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &orgs) != nil || len(orgs) != 1 || orgs[0].ID != orgB {
		t.Fatalf("orgs list = %d %q", r.code, r.stdout)
	}
	if api.auth[0] != "Bearer "+testKey {
		t.Errorf("authorization = %q", api.auth[0])
	}
}

func TestOrgResolution(t *testing.T) {
	keyMe := strings.Replace(twoOrgs, `"api_key":null`, `"api_key":{"id":"k","org_id":"`+orgB+`"}`, 1)
	cases := []struct {
		name    string
		key     bool
		me      string
		env     string
		args    []string
		wantOrg string
		wantErr string
		// wantHint is in the error's hint; "--org" when empty.
		wantHint string
	}{
		{name: "flag", me: twoOrgs, env: orgA, args: []string{"--org", orgB}, wantOrg: orgB},
		{name: "environment", me: twoOrgs, env: orgA, wantOrg: orgA},
		{name: "only organisation", me: oneOrg, wantOrg: orgA},
		{name: "several", me: twoOrgs, wantErr: "org_required"},
		{name: "none", me: `{"organisations":[]}`, wantErr: "org_required", wantHint: "console"},
		{name: "api key", key: true, me: keyMe, wantOrg: orgB},
		{name: "api key, flag", key: true, me: keyMe, args: []string{"--org", orgA}, wantOrg: orgA},
		{name: "api key, older API", key: true, me: twoOrgs, wantErr: "org_required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := newFakeAPI(t, map[string]http.HandlerFunc{
				"GET /api/v1/me": reply(200, tc.me),
				"GET /api/v1/orgs/" + orgA + "/filesystems": reply(200, filesystems),
				"GET /api/v1/orgs/" + orgB + "/filesystems": reply(200, filesystems),
			})
			if tc.key {
				useAPIKey(t)
			} else {
				useLogin(t) // after the fake API: the login is for its URL
			}
			t.Setenv("MOUNTABLE_ORG", tc.env)
			r := runCLI(t, append([]string{"fs", "list", "--json"}, tc.args...)...)
			if tc.wantErr != "" {
				e := jsonError(t, r)
				if tc.wantHint == "" {
					tc.wantHint = "--org"
				}
				if r.code != 1 || e.Code != tc.wantErr || !strings.Contains(e.Hint, tc.wantHint) {
					t.Fatalf("got %d %+v", r.code, e)
				}
				return
			}
			calls := api.called()
			if r.code != 0 || calls[len(calls)-1] != "GET /api/v1/orgs/"+tc.wantOrg+"/filesystems" {
				t.Fatalf("got %d %v %q", r.code, calls, r.stdout+r.stderr)
			}
		})
	}
}

func TestFilesystemCommands(t *testing.T) {
	useAPIKey(t)
	created := `{"id":"fsnew","name":"my data","byte_quota":10,"entry_quota":5,"created_at":"2026-10-01T00:00:00Z"}`
	usage := `{"retained_bytes":1536,"byte_quota":10737418240,"entries":3,"entry_quota":1000,"bytes_read_30d":0,"bytes_written_30d":2048}`
	routes := map[string]http.HandlerFunc{
		"GET /api/v1/orgs/" + orgA + "/filesystems":             reply(200, filesystems),
		"POST /api/v1/orgs/" + orgA + "/filesystems":            reply(201, created),
		"GET /api/v1/orgs/" + orgA + "/filesystems/fsone/usage": reply(200, usage),
	}
	cases := []struct {
		args     []string
		wantJSON string
		wantText string
	}{
		{[]string{"fs", "list"}, filesystems, "fsone  data"},
		{[]string{"fs", "create", "my data"}, created, "fsnew\n"},
		{[]string{"fs", "usage", "fsone"}, usage, "stored   1.5 KiB of 10.0 GiB\nfiles    3 of 1000\nread     0 B in 30 days\nwritten  2.0 KiB in 30 days\n"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			api := newFakeAPI(t, routes)
			t.Setenv("MOUNTABLE_ORG", orgA)
			r := runCLI(t, append(tc.args, "--json")...)
			if r.code != 0 || strings.TrimSpace(r.stdout) != tc.wantJSON || r.stderr != "" {
				t.Fatalf("--json: %d %q %q", r.code, r.stdout, r.stderr)
			}
			r = runCLI(t, tc.args...)
			if r.code != 0 || !strings.Contains(r.stdout, tc.wantText) {
				t.Fatalf("text: %d %q", r.code, r.stdout)
			}
			if tc.args[1] == "create" && api.bodies[0]["name"] != "my data" {
				t.Errorf("create body = %v", api.bodies[0])
			}
		})
	}
}

func TestSessionCommands(t *testing.T) {
	useAPIKey(t)
	sessions := `[{"id":"s1","filesystem_id":"fsone","mode":"rw","state":"active","created_by":"key:k","expires_at":"2026-10-02T00:00:00Z","created_at":"2026-10-01T00:00:00Z"}]`
	api := newFakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/v1/orgs/" + orgA + "/filesystems/fsone/mount-sessions": reply(200, sessions),
		"DELETE /api/v1/mount-sessions/s1":                               reply(204, ""),
	})
	t.Setenv("MOUNTABLE_ORG", orgA)

	r := runCLI(t, "sessions", "list", "fsone", "--json")
	if r.code != 0 || strings.TrimSpace(r.stdout) != sessions {
		t.Fatalf("sessions list --json = %d %q", r.code, r.stdout)
	}
	r = runCLI(t, "sessions", "list", "fsone")
	if r.code != 0 || !strings.Contains(r.stdout, "s1  rw    active") {
		t.Fatalf("sessions list = %d %q", r.code, r.stdout)
	}
	r = runCLI(t, "sessions", "revoke", "s1", "--json")
	if r.code != 0 || strings.TrimSpace(r.stdout) != `{"id":"s1","revoked":true}` {
		t.Fatalf("sessions revoke --json = %d %q", r.code, r.stdout)
	}
	r = runCLI(t, "sessions", "revoke", "s1")
	if r.code != 0 || r.stdout != "revoked s1\n" {
		t.Fatalf("sessions revoke = %d %q", r.code, r.stdout)
	}
	if calls := api.called(); calls[len(calls)-1] != "DELETE /api/v1/mount-sessions/s1" {
		t.Fatalf("calls = %v", calls)
	}
}

func session(id, state, ticket string) string {
	t := "null"
	if ticket != "" {
		t = `"` + ticket + `"`
	}
	return `{"id":"` + id + `","filesystem_id":"fsone","mode":"rw","state":"` + state +
		`","created_by":"key:k","expires_at":"2026-10-02T00:00:00Z","created_at":"2026-10-01T00:00:00Z","ticket":` + t +
		`,"ticket_expires_at":"2026-10-01T00:05:00Z"}`
}

// The generated idempotency key is on stderr before the request is sent.
func TestTicketCreatePrintsTheKeyBeforeSending(t *testing.T) {
	useAPIKey(t)
	var stdout, stderr syncBuffer
	var keyAnnounced bool
	newFakeAPI(t, map[string]http.HandlerFunc{
		"POST /api/v1/mount-sessions": func(w http.ResponseWriter, r *http.Request) {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			keyAnnounced = body["idempotency_key"] != "" && strings.Contains(stderr.String(), body["idempotency_key"])
			reply(201, session("s1", "requested", "mtbltk_theticket"))(w, r)
		},
	})
	r := runWith(t, &stdout, &stderr, "ticket", "create", "fsone")
	if r.code != 0 || r.stdout != "mtbltk_theticket\n" {
		t.Fatalf("ticket create = %d %q %q", r.code, r.stdout, r.stderr)
	}
	if !keyAnnounced {
		t.Fatalf("the key was not on stderr before sending: %q", r.stderr)
	}
}

func TestTicketCreateWithKey(t *testing.T) {
	useAPIKey(t)
	api := newFakeAPI(t, map[string]http.HandlerFunc{
		"POST /api/v1/mount-sessions": reply(201, session("s1", "requested", "mtbltk_theticket")),
	})
	r := runCLI(t, "ticket", "create", "fsone", "--ro", "--idempotency-key", "job-42", "--json")
	if r.code != 0 || strings.TrimSpace(r.stdout) != session("s1", "requested", "mtbltk_theticket") {
		t.Fatalf("ticket create = %d %q", r.code, r.stdout)
	}
	if strings.Contains(r.stderr, "job-42") {
		t.Errorf("a given key was announced: %q", r.stderr)
	}
	if b := api.bodies[0]; b["idempotency_key"] != "job-42" || b["mode"] != "ro" || b["filesystem_id"] != "fsone" {
		t.Errorf("body = %v", b)
	}
}

// A replay whose session is still requested lost its ticket: the session is
// revoked and replaced under a fresh key.
func TestTicketReplayRequestedReplacesTheSession(t *testing.T) {
	useAPIKey(t)
	api := newFakeAPI(t, map[string]http.HandlerFunc{
		"POST /api/v1/mount-sessions": func(w http.ResponseWriter, r *http.Request) {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["idempotency_key"] == "job-42" {
				reply(201, session("old", "requested", ""))(w, r)
				return
			}
			reply(201, session("new", "requested", "mtbltk_fresh"))(w, r)
		},
		"DELETE /api/v1/mount-sessions/old": reply(204, ""),
	})
	r := runCLI(t, "ticket", "create", "fsone", "--idempotency-key", "job-42", "--json")
	var out map[string]any
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &out) != nil {
		t.Fatalf("ticket create = %d %q %q", r.code, r.stdout, r.stderr)
	}
	if out["id"] != "new" || out["ticket"] != "mtbltk_fresh" || out["replaced_session_id"] != "old" {
		t.Fatalf("result = %v", out)
	}
	want := []string{"POST /api/v1/mount-sessions", "DELETE /api/v1/mount-sessions/old", "POST /api/v1/mount-sessions"}
	if calls := api.called(); strings.Join(calls, ",") != strings.Join(want, ",") {
		t.Fatalf("calls = %v", calls)
	}
	fresh := api.bodies[2]["idempotency_key"]
	if fresh == "" || fresh == "job-42" || !strings.Contains(r.stderr, fresh) {
		t.Fatalf("fresh key %q not announced: %q", fresh, r.stderr)
	}

	r = runCLI(t, "ticket", "create", "fsone", "--idempotency-key", "job-42")
	if r.code != 0 || r.stdout != "mtbltk_fresh\n" || !strings.Contains(r.stderr, "old") || !strings.Contains(r.stderr, "new") {
		t.Fatalf("text: %d %q %q", r.code, r.stdout, r.stderr)
	}
}

// A replay whose ticket was used revokes nothing and fails with
// ticket_already_used, carrying the existing session.
func TestTicketReplayActiveFails(t *testing.T) {
	for _, state := range []string{"active", "ended"} {
		t.Run(state, func(t *testing.T) {
			useAPIKey(t)
			api := newFakeAPI(t, map[string]http.HandlerFunc{
				"POST /api/v1/mount-sessions": reply(201, session("s1", state, "")),
			})
			r := runCLI(t, "ticket", "create", "fsone", "--idempotency-key", "job-42", "--json")
			e := jsonError(t, r)
			if r.code != 1 || e.Code != "ticket_already_used" || !strings.Contains(e.Hint, "idempotency-key") {
				t.Fatalf("got %d %+v", r.code, e)
			}
			var s map[string]any
			if json.Unmarshal(e.Session, &s) != nil || s["id"] != "s1" || s["state"] != state {
				t.Fatalf("session = %s", e.Session)
			}
			if calls := api.called(); len(calls) != 1 {
				t.Fatalf("calls = %v", calls)
			}
			r = runCLI(t, "ticket", "create", "fsone", "--idempotency-key", "job-42")
			if r.code != 1 || r.stdout != "" || !strings.Contains(r.stderr, "ticket_already_used") || !strings.Contains(r.stderr, "s1") {
				t.Fatalf("text: %d %q %q", r.code, r.stdout, r.stderr)
			}
		})
	}
}

func TestErrorMapping(t *testing.T) {
	cases := []struct {
		name       string
		handler    http.HandlerFunc
		code       string
		hint       string
		retryAfter int
	}{
		{"unauthenticated", reply(401, `{"detail":{"code":"unauthenticated"}}`), "unauthenticated", "run `mountable login` or set `MOUNTABLE_API_KEY`", 0},
		{"payment", reply(402, `{"detail":{"code":"payment_required","url":"https://mountable.io/console/billing"}}`), "payment_required", "https://mountable.io/console/billing", 0},
		{"not found", reply(404, `{"detail":{"code":"not_found"}}`), "not_found", "check the ID with `mountable fs list`", 0},
		{"no grant", reply(403, `{"detail":{"code":"no_grant"}}`), "no_grant", "ask an owner or admin for a grant, or use `--ro`", 0},
		{"key forbidden", reply(403, `{"detail":{"code":"api_key_forbidden"}}`), "api_key_forbidden", "use a person's login (`mountable login`) or the console", 0},
		{"rate limited", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "7")
			reply(429, `{"detail":{"code":"rate_limited"}}`)(w, r)
		}, "rate_limited", "wait `retry_after` seconds, then retry", 7},
		{"conflict", reply(409, `{"detail":{"code":"idempotency_conflict"}}`), "idempotency_conflict", "wait a moment, then retry with the same idempotency key", 0},
		{"invalid", reply(422, `{"detail":[{"loc":["body","mode"],"msg":"bad value mtbl_leakedsecret","type":"x"}]}`), "invalid_request", "check the command's arguments", 0},
		{"no code", reply(502, `<html>bad gateway</html>`), "api_error", "retry later", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useAPIKey(t)
			newFakeAPI(t, map[string]http.HandlerFunc{"POST /api/v1/mount-sessions": tc.handler})
			r := runCLI(t, "ticket", "create", "fsone", "--idempotency-key", "k", "--json")
			e := jsonError(t, r)
			if r.code != 1 || e.Code != tc.code || e.Hint != tc.hint || e.RetryAfter != tc.retryAfter || e.Message == "" {
				t.Fatalf("got %d %+v", r.code, e)
			}
			if strings.Contains(r.stdout, "mtbl_leakedsecret") {
				t.Fatalf("not redacted: %s", r.stdout)
			}
			r = runCLI(t, "ticket", "create", "fsone", "--idempotency-key", "k")
			if r.code != 1 || r.stdout != "" || !strings.Contains(r.stderr, "("+tc.code+")") || !strings.Contains(r.stderr, "hint: "+tc.hint) {
				t.Fatalf("text: %d %q %q", r.code, r.stdout, r.stderr)
			}
		})
	}
}

func TestNetworkErrorNamesTheKeyToRetryWith(t *testing.T) {
	useAPIKey(t)
	api := newFakeAPI(t, nil)
	api.routes = map[string]http.HandlerFunc{"POST /api/v1/mount-sessions": func(w http.ResponseWriter, _ *http.Request) {
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}}
	r := runCLI(t, "ticket", "create", "fsone", "--idempotency-key", "job-42", "--json")
	e := jsonError(t, r)
	if r.code != 1 || e.Code != "network_error" || !strings.Contains(e.Hint, "job-42") {
		t.Fatalf("got %d %+v", r.code, e)
	}
	// A dead API is a network error too.
	t.Setenv("MOUNTABLE_API_URL", "http://127.0.0.1:1")
	t.Setenv("MOUNTABLE_ORG", orgA)
	r = runCLI(t, "fs", "list", "--json")
	if e := jsonError(t, r); r.code != 1 || e.Code != "network_error" {
		t.Fatalf("got %d %+v", r.code, e)
	}
}

func TestNotSignedIn(t *testing.T) {
	t.Setenv("MOUNTABLE_API_KEY", "")
	isolateConfig(t)
	newFakeAPI(t, nil)
	r := runCLI(t, "fs", "list", "--json")
	if e := jsonError(t, r); r.code != 1 || e.Code != "unauthenticated" || !strings.Contains(e.Hint, "MOUNTABLE_API_KEY") {
		t.Fatalf("got %d %+v", r.code, e)
	}
}

func TestUsageErrorsExitTwo(t *testing.T) {
	useAPIKey(t)
	api := newFakeAPI(t, nil)
	for _, args := range [][]string{
		{},
		{"nonsense"},
		{"fs"},
		{"fs", "create"},
		{"fs", "create", "a", "b"},
		{"fs", "list", "--bogus"},
		{"ticket", "create"},
		{"sessions", "revoke"},
		{"version", "extra"},
		{"mount", "--ticket-stdin"},
		{"unmount"},
	} {
		r := runCLI(t, args...)
		if r.code != 2 || r.stdout != "" || r.stderr == "" {
			t.Errorf("%v = %d %q %q", args, r.code, r.stdout, r.stderr)
		}
	}
	r := runCLI(t, "fs", "create", "--json")
	if e := jsonError(t, r); r.code != 2 || e.Code != "usage" || !strings.Contains(e.Message, "fs create NAME") {
		t.Fatalf("got %d %+v", r.code, e)
	}
	if calls := api.called(); len(calls) != 0 {
		t.Fatalf("usage errors called the API: %v", calls)
	}
}

func TestFlagsMayFollowArguments(t *testing.T) {
	useAPIKey(t)
	api := newFakeAPI(t, map[string]http.HandlerFunc{
		"POST /api/v1/orgs/" + orgB + "/filesystems": reply(201, `{"id":"fsnew","name":"x"}`),
	})
	r := runCLI(t, "fs", "create", "x", "--json", "--org", orgB)
	if r.code != 0 || !strings.Contains(r.stdout, `"fsnew"`) {
		t.Fatalf("got %d %q %q %v", r.code, r.stdout, r.stderr, api.called())
	}
}

func TestVersionAndHelp(t *testing.T) {
	r := runCLI(t, "version", "--json")
	if r.code != 0 || strings.TrimSpace(r.stdout) != `{"version":"`+version+`"}` {
		t.Fatalf("version --json = %d %q", r.code, r.stdout)
	}
	r = runCLI(t, "help")
	if r.code != 0 || !strings.Contains(r.stdout, "mountable ticket create") {
		t.Fatalf("help = %d %q", r.code, r.stdout)
	}
}

// With --json, a mount that fails before it is mounted writes one JSON
// error line.
func TestMountJSONReportsStartupErrors(t *testing.T) {
	newFakeAPI(t, map[string]http.HandlerFunc{
		"POST /api/v1/mount-sessions:exchange": reply(404, `{"detail":{"code":"ticket_invalid"}}`),
	})
	isolateConfig(t)
	stdin, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdin
	os.Stdin = stdin
	defer func() { os.Stdin = saved }()
	fmt.Fprintln(w, "mtbltk_usedticket")
	w.Close()

	r := runCLI(t, "mount", "--ticket-stdin", "--json", t.TempDir())
	e := jsonError(t, r)
	if r.code != 1 || e.Code != "ticket_invalid" || strings.Count(r.stdout, "\n") != 1 {
		t.Fatalf("got %d %q", r.code, r.stdout)
	}
	if strings.Contains(r.stdout+r.stderr, "mtbltk_usedticket") {
		t.Fatal("the ticket leaked")
	}
}

func TestRedact(t *testing.T) {
	for in, want := range map[string]string{
		"key mtbl_abc-DEF_1 refused":    "key mtbl_[redacted] refused",
		"ticket mtbltk_xyz":             "ticket mtbltk_[redacted]",
		"tokens mtblat_a and mtblrt_b.": "tokens mtblat_[redacted] and mtblrt_[redacted].",
		"nothing secret here":           "nothing secret here",
	} {
		if got := redact(in); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEndReason(t *testing.T) {
	failed := fmt.Errorf("could not unmount")
	cases := []struct {
		ended event
		err   error
		want  string
	}{
		{eventUnmounted, nil, "unmount"},
		{eventSignal, nil, "signal"},
		{eventSignal, failed, "error"},
		{eventUnmounted, failed, "error"},
		{eventRevoked, failed, "revoked"},
		{eventExpired, failed, "expired"},
	}
	for _, tc := range cases {
		l := &lifecycle{ended: tc.ended}
		if got := l.endReason(tc.err); got != tc.want {
			t.Errorf("endReason(%d, %v) = %q, want %q", tc.ended, tc.err, got, tc.want)
		}
	}
}

// With --json, login's instructions go to stderr and stdout carries only
// the JSON result.
func TestLoginAndLogoutJSON(t *testing.T) {
	newFakeAPI(t, map[string]http.HandlerFunc{
		"POST /auth/device/code":  reply(200, `{"device_code":"d","user_code":"ABCD-EFGH","verification_uri_complete":"https://example.com/x","expires_in":60,"interval":0}`),
		"POST /auth/device/token": reply(200, `{"access_token":"`+testLogin+`","refresh_token":"mtblrt_refreshsecret","expires_in":3600}`),
		"POST /auth/cli/sign-out": reply(204, ""),
	})
	t.Setenv("MOUNTABLE_API_KEY", "")
	isolateConfig(t)
	r := runCLI(t, "login", "--json")
	if r.code != 0 || r.stdout != `{"signed_in":true}`+"\n" || !strings.Contains(r.stderr, "ABCD-EFGH") {
		t.Fatalf("login --json = %d %q %q", r.code, r.stdout, r.stderr)
	}
	r = runCLI(t, "logout", "--json")
	if r.code != 0 || r.stdout != `{"signed_out":true}`+"\n" {
		t.Fatalf("logout --json = %d %q %q", r.code, r.stdout, r.stderr)
	}
	r = runCLI(t, "logout", "--json")
	if e := jsonError(t, r); r.code != 1 || e.Code != "unauthenticated" {
		t.Fatalf("second logout = %d %+v", r.code, e)
	}
}
