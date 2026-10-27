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
	// testJWT is an access token shaped like the API's JWTs.
	testJWT = "mtblat_eyJhbGciOi.eyJzdWIi.c2lnbmF0dXJl"
	// testGeneration is the generation of useLogin's login.
	testGeneration = "gen-1"
	// escapedSession hides secrets behind JSON escapes: in a value, behind
	// an escaped prefix, and in a key.
	escapedSession = `{"id":"s1","state":"active","ticket":null,` +
		`"created_by":"mtblat_eyJhead\u002epayloadsecret\u002esignaturesecret",` +
		`"note":"mtbl\u005fhiddensecret","mtbl_keyinkeysecret":1}`
	orgA = "11111111-1111-1111-1111-111111111111"
	orgB = "22222222-2222-2222-2222-222222222222"
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
	// json holds each request's body decoded with its value types.
	json []map[string]any
	auth []string
	url  string
}

func newFakeAPI(t *testing.T, routes map[string]http.HandlerFunc) *fakeAPI {
	f := &fakeAPI{t: t, routes: routes}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		route := r.Method + " " + r.URL.Path
		var body map[string]string
		var typed map[string]any
		data, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(data, &body)
		_ = json.Unmarshal(data, &typed)
		r.Body = io.NopCloser(bytes.NewReader(data))
		f.mu.Lock()
		f.calls = append(f.calls, route)
		f.bodies = append(f.bodies, body)
		f.json = append(f.json, typed)
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
	if err := saveCredentials(tokenResponse{AccessToken: testLogin, RefreshToken: "mtblrt_refreshsecret", ExpiresIn: 3600}, testGeneration); err != nil {
		t.Fatal(err)
	}
}

// captureDiagnostics collects diagnostics until the test ends.
func captureDiagnostics(t *testing.T) *syncBuffer {
	buf := &syncBuffer{}
	diagnosticsMu.Lock()
	saved := diagnostics
	diagnostics = buf
	diagnosticsMu.Unlock()
	t.Cleanup(func() {
		diagnosticsMu.Lock()
		diagnostics = saved
		diagnosticsMu.Unlock()
	})
	return buf
}

func isolateConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("AppData", t.TempDir()) // Windows
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
	for _, secret := range []string{testKey, testLogin, testJWT, "eyJzdWIi", "mtblrt_refreshsecret", "mtblrt_rotated"} {
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

// --max-duration is sent in seconds, also when a lost ticket's session is
// replaced; without it the API's default applies.
func TestTicketCreateMaxDuration(t *testing.T) {
	useAPIKey(t)
	api := newFakeAPI(t, map[string]http.HandlerFunc{
		"POST /api/v1/mount-sessions": func(w http.ResponseWriter, r *http.Request) {
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["idempotency_key"] == "lost" {
				reply(201, session("old", "requested", ""))(w, r)
				return
			}
			reply(201, session("s1", "requested", "mtbltk_theticket"))(w, r)
		},
		"DELETE /api/v1/mount-sessions/old": reply(204, ""),
	})
	if r := runCLI(t, "ticket", "create", "fsone", "--max-duration", "720h", "--json"); r.code != 0 {
		t.Fatalf("ticket create = %d %q %q", r.code, r.stdout, r.stderr)
	}
	if got := api.json[0]["max_duration_seconds"]; got != float64(720*3600) {
		t.Fatalf("max_duration_seconds = %v", got)
	}
	if r := runCLI(t, "ticket", "create", "fsone", "--json"); r.code != 0 {
		t.Fatalf("ticket create = %d %q %q", r.code, r.stdout, r.stderr)
	}
	if _, ok := api.json[1]["max_duration_seconds"]; ok {
		t.Fatalf("sent max_duration_seconds without --max-duration: %v", api.json[1])
	}
	if r := runCLI(t, "ticket", "create", "fsone", "--idempotency-key", "lost", "--max-duration", "12h", "--json"); r.code != 0 {
		t.Fatalf("ticket create = %d %q %q", r.code, r.stdout, r.stderr)
	}
	if got := api.json[4]["max_duration_seconds"]; got != float64(12*3600) {
		t.Fatalf("replacement max_duration_seconds = %v (%v)", got, api.called())
	}
	for _, d := range []string{"-1h", "0s", "0", "1500ms", "soon"} {
		r := runCLI(t, "ticket", "create", "fsone", "--max-duration", d)
		if r.code != 2 || !strings.Contains(r.stderr, "--max-duration") {
			t.Errorf("--max-duration %s = %d %q", d, r.code, r.stderr)
		}
	}
	if calls := api.called(); len(calls) != 5 {
		t.Fatalf("calls = %v", calls)
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
		{"server error", reply(502, `<html>bad gateway</html>`), "outcome_unknown", "retry with the same idempotency key k", 0},
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
	if r.code != 1 || e.Code != "network_error" || e.IdempotencyKey != "job-42" || !strings.Contains(e.Hint, "job-42") {
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
	r = runCLI(t, "help", "--json")
	var help map[string]string
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &help) != nil || !strings.Contains(help["usage"], "mountable ticket create") {
		t.Fatalf("help --json = %d %q", r.code, r.stdout)
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

	r := runCLI(t, "mount", "--ticket-stdin", "--json", testMountPoint(t))
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
		"key mtbl_abc-DEF_1 refused": "key mtbl_[redacted] refused",
		"ticket mtbltk_xyz":          "ticket mtbltk_[redacted]",
		// A trailing dot is consumed with the secret, as the API specifies.
		"tokens mtblat_a and mtblrt_b.": "tokens mtblat_[redacted] and mtblrt_[redacted]",
		"jwt " + testJWT + " refused":   "jwt mtblat_[redacted] refused",
		"jwt " + testJWT + ".":          "jwt mtblat_[redacted]",
		"(" + testJWT + "), then":       "(mtblat_[redacted]), then",
		"odd mtblat_a..b.c-d_e":         "odd mtblat_[redacted]",
		"jwt mtblat_.payload.signature": "jwt mtblat_[redacted]",
		"refresh mtblrt_... refused":    "refresh mtblrt_[redacted] refused",
		"secret " + webhookSecret + ".": "secret whsec_[redacted].",
		"(whsec_a+b/c=), then":          "(whsec_[redacted]), then",
		"nothing secret here":           "nothing secret here",
	} {
		if got := redact(in); got != want {
			t.Errorf("redact(%q) = %q, want %q", in, got, want)
		}
	}
}

// A JWT-shaped token in an error is redacted whole, payload and signature
// included, in plain and JSON output.
func TestJWTRedactedInErrors(t *testing.T) {
	useAPIKey(t)
	newFakeAPI(t, map[string]http.HandlerFunc{
		"POST /api/v1/mount-sessions": reply(422, `{"detail":[{"loc":["body","mode"],"msg":"bad value `+testJWT+`.","type":"x"}]}`),
	})
	for _, args := range [][]string{
		{"ticket", "create", "fsone", "--idempotency-key", "k", "--json"},
		{"ticket", "create", "fsone", "--idempotency-key", "k"},
	} {
		r := runCLI(t, args...)
		out := r.stdout + r.stderr
		if r.code != 1 || strings.Contains(out, "eyJ") || strings.Contains(out, "c2lnbmF0dXJl") || !strings.Contains(out, "bad value mtblat_[redacted]") {
			t.Fatalf("%v: %d %q %q", args, r.code, r.stdout, r.stderr)
		}
	}
}

// leaksEscapedSecret reports whether s shows a secret from escapedSession.
func leaksEscapedSecret(s string) bool {
	for _, secret := range []string{"payloadsecret", "signaturesecret", "hiddensecret", "keyinkeysecret"} {
		if strings.Contains(s, secret) {
			return true
		}
	}
	return false
}

// Every field of a reported error is redacted: the API's code, the
// idempotency key and the session carried by ticket_already_used.
func TestEveryErrorFieldIsRedacted(t *testing.T) {
	useAPIKey(t)
	newFakeAPI(t, map[string]http.HandlerFunc{
		"POST /api/v1/mount-sessions": func(w http.ResponseWriter, r *http.Request) {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["idempotency_key"] == "mtbl_keysecret" {
				reply(201, escapedSession)(w, r)
				return
			}
			reply(400, `{"detail":{"code":"bad_`+testJWT+`"}}`)(w, r)
		},
	})
	for _, args := range [][]string{
		{"ticket", "create", "fsone", "--idempotency-key", "mtbl_keysecret", "--json"},
		{"ticket", "create", "fsone", "--idempotency-key", "mtbl_keysecret"},
		{"ticket", "create", "fsone", "--idempotency-key", "k", "--json"},
		{"ticket", "create", "fsone", "--idempotency-key", "k"},
	} {
		r := runCLI(t, args...)
		out := r.stdout + r.stderr
		if r.code != 1 || leaksEscapedSecret(out) || strings.Contains(out, "keysecret") || strings.Contains(out, "eyJ") || !strings.Contains(out, "_[redacted]") {
			t.Fatalf("%v: %d %q %q", args, r.code, r.stdout, r.stderr)
		}
		if strings.HasSuffix(args[len(args)-1], "json") {
			if e := jsonError(t, r); args[4] == "mtbl_keysecret" && (e.IdempotencyKey != "mtbl_[redacted]" || !json.Valid(e.Session) || !strings.Contains(string(e.Session), `"created_by":"mtblat_[redacted]"`) || !strings.Contains(string(e.Session), `"note":"mtbl_[redacted]"`)) {
				t.Fatalf("%v: %+v", args, e)
			}
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

// cutShort answers 201 but drops the connection partway through the body.
func cutShort(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Length", "1000")
	w.WriteHeader(201)
	fmt.Fprint(w, `{"id":"new","ticket":"mtbltk_`)
}

// Every uncertain outcome of a ticket request reports the key that request
// was sent with, including the fresh key of a replacement.
func TestUncertainTicketOutcomesNameTheKey(t *testing.T) {
	cases := []struct {
		name  string
		reply http.HandlerFunc
	}{
		{"body cut short", cutShort},
		{"undecodable body", reply(201, `{"id":`)},
		{"server error", reply(500, `{"detail":"boom"}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useAPIKey(t)
			api := newFakeAPI(t, map[string]http.HandlerFunc{
				"POST /api/v1/mount-sessions": func(w http.ResponseWriter, r *http.Request) {
					var body map[string]string
					_ = json.NewDecoder(r.Body).Decode(&body)
					if body["idempotency_key"] == "job-42" {
						reply(201, session("old", "requested", ""))(w, r)
						return
					}
					tc.reply(w, r)
				},
				"DELETE /api/v1/mount-sessions/old": reply(204, ""),
			})
			r := runCLI(t, "ticket", "create", "fsone", "--idempotency-key", "job-42", "--json")
			e := jsonError(t, r)
			fresh := api.bodies[len(api.bodies)-1]["idempotency_key"]
			if r.code != 1 || fresh == "job-42" || e.IdempotencyKey != fresh || !strings.Contains(e.Hint, fresh) ||
				(e.Code != "outcome_unknown" && e.Code != "network_error") {
				t.Fatalf("got %d %+v (fresh key %q)", r.code, e, fresh)
			}
		})
	}
}

// A missing mount directory is reported before the ticket is exchanged.
func TestMountChecksTheDirectoryBeforeUsingTheTicket(t *testing.T) {
	api := newFakeAPI(t, nil)
	isolateConfig(t)
	stdin, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdin
	os.Stdin = stdin
	defer func() { os.Stdin = saved }()
	fmt.Fprintln(w, "mtbltk_unusedticket")
	w.Close()

	r := runCLI(t, "mount", "--ticket-stdin", "--json", missingMountPoint(t))
	e := jsonError(t, r)
	if r.code != 1 || e.Code != "mount_failed" || !strings.Contains(e.Hint, "mkdir") {
		t.Fatalf("got %d %+v", r.code, e)
	}
	if calls := api.called(); len(calls) != 0 {
		t.Fatalf("the ticket was used: %v", calls)
	}
}

const limitedFS = `{"id":"fsone","name":"user-42","byte_quota":10737418240,"entry_quota":100000,` +
	`"spending_cap_cents":500,"mutations_per_minute":null,"created_at":"2026-10-01T00:00:00Z"}`

// fs create and fs update send only the limits given; update's none sends
// null for the spending cap and the mutation rate.
func TestFilesystemLimits(t *testing.T) {
	useAPIKey(t)
	fsPath := "/api/v1/orgs/" + orgA + "/filesystems"
	api := newFakeAPI(t, map[string]http.HandlerFunc{
		"POST " + fsPath:             reply(201, limitedFS),
		"PATCH " + fsPath + "/fsone": reply(200, limitedFS),
	})
	t.Setenv("MOUNTABLE_ORG", orgA)
	cases := []struct {
		args []string
		want map[string]any
	}{
		{[]string{"fs", "create", "user-42"}, map[string]any{"name": "user-42"}},
		{[]string{"fs", "create", "user-42", "--spending-cap-cents", "500", "--mutations-per-minute", "120", "--byte-quota", "10737418240", "--entry-quota", "100000"},
			map[string]any{"name": "user-42", "spending_cap_cents": float64(500), "mutations_per_minute": float64(120), "byte_quota": float64(10737418240), "entry_quota": float64(100000)}},
		{[]string{"fs", "update", "fsone", "--spending-cap-cents", "0"}, map[string]any{"spending_cap_cents": float64(0)}},
		{[]string{"fs", "update", "fsone", "--spending-cap-cents", "none", "--mutations-per-minute", "none", "--entry-quota", "20000"},
			map[string]any{"spending_cap_cents": nil, "mutations_per_minute": nil, "entry_quota": float64(20000)}},
	}
	for i, tc := range cases {
		r := runCLI(t, append(tc.args, "--json")...)
		if r.code != 0 || strings.TrimSpace(r.stdout) != limitedFS {
			t.Fatalf("%v = %d %q %q", tc.args, r.code, r.stdout, r.stderr)
		}
		if got := api.json[i]; fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("%v sent %v, want %v", tc.args, got, tc.want)
		}
	}
	r := runCLI(t, "fs", "update", "fsone", "--byte-quota", "2147483648")
	if r.code != 0 || !strings.Contains(r.stdout, "spending cap          500 cents\n") || !strings.Contains(r.stdout, "mutations per minute  none\n") {
		t.Fatalf("fs update = %d %q", r.code, r.stdout)
	}
}

func TestFilesystemGet(t *testing.T) {
	useAPIKey(t)
	newFakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/v1/orgs/" + orgA + "/filesystems/fsone": reply(200, limitedFS),
	})
	t.Setenv("MOUNTABLE_ORG", orgA)
	r := runCLI(t, "fs", "get", "fsone", "--json")
	if r.code != 0 || strings.TrimSpace(r.stdout) != limitedFS || r.stderr != "" {
		t.Fatalf("fs get --json = %d %q %q", r.code, r.stdout, r.stderr)
	}
	r = runCLI(t, "fs", "get", "fsone")
	want := "id                    fsone\n" +
		"name                  user-42\n" +
		"created               2026-10-01T00:00:00Z\n" +
		"spending cap          500 cents\n" +
		"mutations per minute  none\n" +
		"byte quota            10.0 GiB\n" +
		"entry quota           100000\n"
	if r.code != 0 || r.stdout != want {
		t.Fatalf("fs get = %d %q", r.code, r.stdout)
	}
}

// Bad limit values, none for a quota or on create, and an update without
// a limit are usage errors, refused before any request.
func TestFilesystemLimitUsageErrors(t *testing.T) {
	useAPIKey(t)
	api := newFakeAPI(t, nil)
	t.Setenv("MOUNTABLE_ORG", orgA)
	for _, args := range [][]string{
		{"fs", "update", "fsone"},
		{"fs", "update", "fsone", "--org", orgA},
		{"fs", "update", "fsone", "--byte-quota", "none"},
		{"fs", "update", "fsone", "--entry-quota", "none"},
		{"fs", "update", "fsone", "--spending-cap-cents", "-1"},
		{"fs", "update", "fsone", "--mutations-per-minute", "1.5"},
		{"fs", "update", "fsone", "--spending-cap-cents", "+5"},
		{"fs", "update", "--spending-cap-cents", "5"},
		{"fs", "create", "x", "--spending-cap-cents", "none"},
		{"fs", "create", "x", "--byte-quota", "10GiB"},
		{"fs", "get"},
	} {
		r := runCLI(t, args...)
		if r.code != 2 || r.stdout != "" || !strings.Contains(r.stderr, "usage: mountable fs") {
			t.Errorf("%v = %d %q %q", args, r.code, r.stdout, r.stderr)
		}
	}
	r := runCLI(t, "fs", "update", "fsone", "--json")
	if e := jsonError(t, r); r.code != 2 || e.Code != "usage" || !strings.Contains(e.Message, "--spending-cap-cents N|none") {
		t.Fatalf("got %d %+v", r.code, e)
	}
	if calls := api.called(); len(calls) != 0 {
		t.Fatalf("usage errors called the API: %v", calls)
	}
}

// The API checks ranges and permissions; its errors reach the caller with
// their code.
func TestFilesystemLimitErrorsPropagate(t *testing.T) {
	cases := []struct {
		name  string
		reply http.HandlerFunc
		code  string
		hint  string
	}{
		{"limited key", reply(403, `{"detail":{"code":"key_limited_to_filesystem","message":"this key is limited to one filesystem"}}`), "key_limited_to_filesystem", "use a key for the whole organisation"},
		{"not an admin", reply(403, `{"detail":{"code":"forbidden"}}`), "forbidden", "ask an owner or admin"},
		{"range", reply(422, `{"detail":[{"loc":["body","mutations_per_minute"],"msg":"Input should be less than or equal to 1000","type":"x"}]}`), "invalid_request", "check the command's arguments"},
		{"quota null", reply(422, `{"detail":{"code":"quota_required","message":"byte_quota cannot be removed"}}`), "quota_required", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useAPIKey(t)
			newFakeAPI(t, map[string]http.HandlerFunc{
				"PATCH /api/v1/orgs/" + orgA + "/filesystems/fsone": tc.reply,
				"POST /api/v1/orgs/" + orgA + "/filesystems":        tc.reply,
			})
			t.Setenv("MOUNTABLE_ORG", orgA)
			for _, args := range [][]string{
				{"fs", "update", "fsone", "--mutations-per-minute", "5000", "--json"},
				{"fs", "create", "x", "--mutations-per-minute", "5000", "--json"},
			} {
				r := runCLI(t, args...)
				e := jsonError(t, r)
				if r.code != 1 || e.Code != tc.code || e.Message == "" || !strings.Contains(e.Hint, tc.hint) {
					t.Fatalf("%v: got %d %+v", args, r.code, e)
				}
			}
		})
	}
}

const (
	webhookSecret = "whsec_c2VjcmV0K2tleS9iYXNlNjQ="
	webhook       = `{"id":"wh1","filesystem_id":"fsone","url":"https://example.com/hooks","path_prefix":"/docs",` +
		`"state":"active","disabled_reason":null,"created_by":"key:k","created_at":"2026-10-01T00:00:00Z",` +
		`"last_success_at":null,"last_failure_at":null,"last_failure":null,"failing_since":null}`
	webhookCreated = `{"id":"wh1","filesystem_id":"fsone","url":"https://example.com/hooks","path_prefix":"/docs",` +
		`"state":"active","disabled_reason":null,"created_by":"key:k","created_at":"2026-10-01T00:00:00Z",` +
		`"last_success_at":null,"last_failure_at":null,"last_failure":null,"failing_since":null,"secret":"` + webhookSecret + `"}`
)

func TestWebhookCommands(t *testing.T) {
	useAPIKey(t)
	hooks := "/api/v1/orgs/" + orgA + "/filesystems/fsone/webhooks"
	failing := strings.Replace(webhook, `"last_failure":null`, `"last_failure":"HTTP 500"`, 1)
	api := newFakeAPI(t, map[string]http.HandlerFunc{
		"POST " + hooks:                        reply(201, webhookCreated),
		"GET " + hooks:                         reply(200, "["+webhook+","+strings.Replace(failing, `"wh1"`, `"wh2"`, 1)+"]"),
		"DELETE " + hooks + "/wh1":             reply(204, ""),
		"POST " + hooks + "/wh1:rotate-secret": reply(200, webhookCreated),
		"POST " + hooks + "/wh1:enable":        reply(200, webhook),
	})
	t.Setenv("MOUNTABLE_ORG", orgA)

	r := runCLI(t, "webhooks", "create", "fsone", "https://example.com/hooks", "--path-prefix", "/docs", "--json")
	if r.code != 0 || strings.TrimSpace(r.stdout) != webhookCreated || r.stderr != "" {
		t.Fatalf("webhooks create --json = %d %q %q", r.code, r.stdout, r.stderr)
	}
	if got := api.json[0]; fmt.Sprint(got) != fmt.Sprint(map[string]any{"url": "https://example.com/hooks", "path_prefix": "/docs"}) {
		t.Errorf("create body = %v", got)
	}
	r = runCLI(t, "webhooks", "create", "fsone", "https://example.com/hooks")
	if r.code != 0 || r.stdout != webhookSecret+"\n" || !strings.Contains(r.stderr, "wh1") || !strings.Contains(r.stderr, "only once") || strings.Contains(r.stderr, "whsec") {
		t.Fatalf("webhooks create = %d %q %q", r.code, r.stdout, r.stderr)
	}
	if _, sent := api.json[1]["path_prefix"]; sent {
		t.Errorf("sent path_prefix without --path-prefix: %v", api.json[1])
	}
	// A prefix given empty is sent as it is; the API decides.
	r = runCLI(t, "webhooks", "create", "fsone", "https://example.com/hooks", "--path-prefix", "", "--json")
	if prefix, sent := api.json[2]["path_prefix"]; r.code != 0 || !sent || prefix != "" {
		t.Errorf("--path-prefix '' sent %v", api.json[2])
	}

	r = runCLI(t, "webhooks", "rotate-secret", "fsone", "wh1")
	if r.code != 0 || r.stdout != webhookSecret+"\n" || !strings.Contains(r.stderr, "wh1") || strings.Contains(r.stderr, "whsec") {
		t.Fatalf("webhooks rotate-secret = %d %q %q", r.code, r.stdout, r.stderr)
	}
	r = runCLI(t, "webhooks", "rotate-secret", "fsone", "wh1", "--json")
	if r.code != 0 || strings.TrimSpace(r.stdout) != webhookCreated {
		t.Fatalf("webhooks rotate-secret --json = %d %q", r.code, r.stdout)
	}

	r = runCLI(t, "webhooks", "list", "fsone", "--json")
	if r.code != 0 || !strings.HasPrefix(r.stdout, "["+webhook) {
		t.Fatalf("webhooks list --json = %d %q", r.code, r.stdout)
	}
	r = runCLI(t, "webhooks", "list", "fsone")
	want := "ID   STATE   PATH-PREFIX  URL                        LAST-FAILURE\n" +
		"wh1  active  /docs        https://example.com/hooks  -\n" +
		"wh2  active  /docs        https://example.com/hooks  HTTP 500\n"
	if r.code != 0 || r.stdout != want {
		t.Fatalf("webhooks list = %d %q", r.code, r.stdout)
	}

	r = runCLI(t, "webhooks", "enable", "fsone", "wh1", "--json")
	if r.code != 0 || strings.TrimSpace(r.stdout) != webhook {
		t.Fatalf("webhooks enable --json = %d %q", r.code, r.stdout)
	}
	r = runCLI(t, "webhooks", "enable", "fsone", "wh1")
	if r.code != 0 || r.stdout != "wh1 active\n" {
		t.Fatalf("webhooks enable = %d %q", r.code, r.stdout)
	}

	r = runCLI(t, "webhooks", "delete", "fsone", "wh1", "--json")
	if r.code != 0 || strings.TrimSpace(r.stdout) != `{"deleted":true,"id":"wh1"}` {
		t.Fatalf("webhooks delete --json = %d %q", r.code, r.stdout)
	}
	r = runCLI(t, "webhooks", "delete", "fsone", "wh1")
	if r.code != 0 || r.stdout != "deleted wh1\n" {
		t.Fatalf("webhooks delete = %d %q", r.code, r.stdout)
	}
	if calls := api.called(); calls[len(calls)-1] != "DELETE "+hooks+"/wh1" {
		t.Fatalf("calls = %v", calls)
	}
}

func TestWebhookUsageErrors(t *testing.T) {
	useAPIKey(t)
	api := newFakeAPI(t, nil)
	for _, args := range [][]string{
		{"webhooks"},
		{"webhooks", "create", "fsone"},
		{"webhooks", "list"},
		{"webhooks", "delete", "fsone"},
		{"webhooks", "rotate-secret", "fsone"},
		{"webhooks", "enable", "fsone", "wh1", "extra"},
	} {
		r := runCLI(t, args...)
		if r.code != 2 || r.stdout != "" || r.stderr == "" {
			t.Errorf("%v = %d %q %q", args, r.code, r.stdout, r.stderr)
		}
	}
	if calls := api.called(); len(calls) != 0 {
		t.Fatalf("usage errors called the API: %v", calls)
	}
}

// Every webhook call needs read-write access; the API's refusal reaches
// the caller, and a secret the API echoes in an error is redacted.
func TestWebhookErrors(t *testing.T) {
	useAPIKey(t)
	hooks := "/api/v1/orgs/" + orgA + "/filesystems/fsone/webhooks"
	noGrant := reply(403, `{"detail":{"code":"no_grant"}}`)
	newFakeAPI(t, map[string]http.HandlerFunc{
		"GET " + hooks:                         noGrant,
		"POST " + hooks + "/wh1:enable":        noGrant,
		"POST " + hooks:                        reply(422, `{"detail":[{"loc":["body","url"],"msg":"bad url, secret `+webhookSecret+` kept","type":"x"}]}`),
		"POST " + hooks + "/wh1:rotate-secret": reply(409, `{"detail":{"code":"conflict","message":"rotating `+webhookSecret+` failed"}}`),
	})
	t.Setenv("MOUNTABLE_ORG", orgA)
	for _, args := range [][]string{{"webhooks", "list", "fsone", "--json"}, {"webhooks", "enable", "fsone", "wh1", "--json"}} {
		r := runCLI(t, args...)
		if e := jsonError(t, r); r.code != 1 || e.Code != "no_grant" {
			t.Fatalf("%v: got %d %+v", args, r.code, e)
		}
	}
	for _, args := range [][]string{
		{"webhooks", "create", "fsone", "http://x", "--json"},
		{"webhooks", "create", "fsone", "http://x"},
		{"webhooks", "rotate-secret", "fsone", "wh1", "--json"},
		{"webhooks", "rotate-secret", "fsone", "wh1"},
	} {
		r := runCLI(t, args...)
		out := r.stdout + r.stderr
		if r.code != 1 || strings.Contains(out, "c2VjcmV0") || !strings.Contains(out, "whsec_[redacted]") {
			t.Fatalf("%v: %d %q %q", args, r.code, r.stdout, r.stderr)
		}
	}
}

const hooksPath = "/api/v1/orgs/" + orgA + "/filesystems/fsone/webhooks"

// hangUp closes the connection without an answer.
func hangUp(w http.ResponseWriter, _ *http.Request) {
	conn, _, _ := w.(http.Hijacker).Hijack()
	conn.Close()
}

// Webhook errors carry webhook next steps: the API's codes, and after an
// unknown outcome, what to check before trying create or rotate-secret
// again. No error is retried.
func TestWebhookErrorHints(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		handler http.HandlerFunc
		code    string
		hint    string
	}{
		{"bad url", []string{"create", "fsone", "http://example.com"}, reply(400, `{"detail":{"code":"bad_url"}}`), "bad_url", "https:// URL on port 443"},
		{"bad prefix", []string{"create", "fsone", "https://example.com", "--path-prefix", "x"}, reply(400, `{"detail":{"code":"bad_path"}}`), "bad_path", "use / or a directory"},
		{"limit", []string{"create", "fsone", "https://example.com"}, reply(409, `{"detail":{"code":"webhook_limit"}}`), "webhook_limit", "mountable webhooks list FS_ID"},
		{"no grant", []string{"create", "fsone", "https://example.com"}, reply(403, `{"detail":{"code":"no_grant"}}`), "no_grant", "read-write grant"},
		{"payment", []string{"create", "fsone", "https://example.com"}, reply(402, `{"detail":{"code":"payment_required","url":"https://mountable.io/console/billing"}}`), "payment_required", "https://mountable.io/console/billing"},
		{"create unknown", []string{"create", "fsone", "https://example.com"}, reply(502, `<html>bad gateway</html>`), "api_error", "look for the URL before creating it again"},
		{"create lost", []string{"create", "fsone", "https://example.com"}, hangUp, "network_error", "look for the URL before creating it again"},
		{"create cut short", []string{"create", "fsone", "https://example.com"}, cutShort, "error", "look for the URL before creating it again"},
		{"not found", []string{"delete", "fsone", "wh1"}, reply(404, `{"detail":{"code":"not_found"}}`), "not_found", "mountable webhooks list FS_ID"},
		{"list no grant", []string{"list", "fsone"}, reply(403, `{"detail":{"code":"no_grant"}}`), "no_grant", "read-write grant"},
		{"revoked", []string{"enable", "fsone", "wh1"}, reply(409, `{"detail":{"code":"access_revoked"}}`), "access_revoked", "create a new webhook"},
		{"rotate unknown", []string{"rotate-secret", "fsone", "wh1"}, reply(503, ""), "api_error", "rotate again"},
		{"rotate lost", []string{"rotate-secret", "fsone", "wh1"}, hangUp, "network_error", "rotate again"},
		{"rotate refused", []string{"rotate-secret", "fsone", "wh1"}, reply(404, `{"detail":{"code":"not_found"}}`), "not_found", "mountable webhooks list FS_ID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useAPIKey(t)
			api := newFakeAPI(t, nil)
			api.routes = map[string]http.HandlerFunc{
				"POST " + hooksPath:                        tc.handler,
				"GET " + hooksPath:                         tc.handler,
				"DELETE " + hooksPath + "/wh1":             tc.handler,
				"POST " + hooksPath + "/wh1:enable":        tc.handler,
				"POST " + hooksPath + "/wh1:rotate-secret": tc.handler,
			}
			t.Setenv("MOUNTABLE_ORG", orgA)
			r := runCLI(t, append(append([]string{"webhooks"}, tc.args...), "--json")...)
			e := jsonError(t, r)
			if r.code != 1 || e.Code != tc.code || !strings.Contains(e.Hint, tc.hint) || e.Message == "" {
				t.Fatalf("got %d %+v", r.code, e)
			}
			r = runCLI(t, append([]string{"webhooks"}, tc.args...)...)
			if r.code != 1 || r.stdout != "" || !strings.Contains(r.stderr, "("+tc.code+")") {
				t.Fatalf("text: %d %q %q", r.code, r.stdout, r.stderr)
			}
			if calls := api.called(); len(calls) != 2 {
				t.Fatalf("calls = %v (an error was retried?)", calls)
			}
		})
	}
}

// The organisation is resolved before a webhook is created or its secret
// rotated, so a failure to resolve it never claims the webhook may exist or
// its secret may have changed.
func TestWebhookCreateResolvesTheOrganisationFirst(t *testing.T) {
	api := newFakeAPI(t, map[string]http.HandlerFunc{"GET /api/v1/me": reply(200, twoOrgs)})
	useLogin(t) // after the fake API: the login is for its URL
	r := runCLI(t, "webhooks", "create", "fsone", "https://example.com", "--json")
	if e := jsonError(t, r); r.code != 1 || e.Code != "org_required" || !strings.Contains(e.Hint, "--org") {
		t.Fatalf("got %d %+v", r.code, e)
	}
	api.routes["GET /api/v1/me"] = hangUp
	for _, args := range [][]string{{"create", "fsone", "https://example.com"}, {"rotate-secret", "fsone", "wh1"}} {
		r = runCLI(t, append(append([]string{"webhooks"}, args...), "--json")...)
		if e := jsonError(t, r); r.code != 1 || e.Code != "network_error" || strings.Contains(e.Hint, "may have") || strings.Contains(e.Hint, "rotate again") {
			t.Fatalf("%v: got %d %+v", args, r.code, e)
		}
	}
	api.routes["GET /api/v1/me"] = reply(200, oneOrg)
	api.routes["POST "+hooksPath] = reply(201, webhookCreated)
	r = runCLI(t, "webhooks", "create", "fsone", "https://example.com")
	if r.code != 0 || r.stdout != webhookSecret+"\n" {
		t.Fatalf("got %d %q %q", r.code, r.stdout, r.stderr)
	}
	if calls := api.called(); calls[len(calls)-1] != "POST "+hooksPath || calls[len(calls)-2] != "GET /api/v1/me" {
		t.Fatalf("calls = %v", calls)
	}
}

// An empty, "." or ".." ID is refused before any request: once the path is
// normalised, webhooks/.. would be the filesystem itself.
func TestWebhookDotSegmentIDsAreRefused(t *testing.T) {
	useAPIKey(t)
	api := newFakeAPI(t, nil) // no MOUNTABLE_ORG: not even /me is asked
	for _, bad := range []string{"", ".", ".."} {
		for _, args := range [][]string{
			{"webhooks", "delete", "fsone", bad},
			{"webhooks", "enable", "fsone", bad},
			{"webhooks", "rotate-secret", "fsone", bad},
			{"webhooks", "list", bad},
			{"webhooks", "create", bad, "https://example.com"},
		} {
			r := runCLI(t, append(args, "--json")...)
			if e := jsonError(t, r); r.code != 1 || e.Code != "invalid_request" || strings.Contains(e.Hint, "may have") {
				t.Errorf("%v = %d %+v", args, r.code, e)
			}
		}
	}
	s := connectMCP(t)
	for _, tool := range []string{"delete_webhook", "enable_webhook"} {
		text, isError := callTool(t, s, tool, map[string]any{"filesystem_id": "fsone", "webhook_id": ".."})
		if !isError || !strings.Contains(text, `"invalid_request"`) {
			t.Errorf("%s = %v %s", tool, isError, text)
		}
	}
	if calls := api.called(); len(calls) != 0 {
		t.Fatalf("calls = %v", calls)
	}
}
