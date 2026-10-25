package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	sessionToken = "mtblst_sessiontokensecret"
	filesPrefix  = "/api/v1/filesystems/fsone"
	written      = `{"path":"/notes/a.md","type":"file","size":5,"etag":"e1","modified_at":"2026-10-01T00:00:00Z"}`
)

// filesRequest is what a files API call carried.
type filesRequest struct {
	uri    string
	header http.Header
	body   string
}

// filesAPI is a fake API with token sessions: it creates ts1 and records
// each files call.
type filesAPI struct {
	*fakeAPI
	mu       sync.Mutex
	requests []filesRequest
}

func newFilesAPI(t *testing.T, routes map[string]http.HandlerFunc) *filesAPI {
	f := &filesAPI{}
	all := map[string]http.HandlerFunc{
		"POST /api/v1/mount-sessions":       reply(201, `{"id":"ts1","filesystem_id":"fsone","mode":"rw","state":"active","token":"`+sessionToken+`","expires_at":"2026-10-01T01:00:00Z"}`),
		"DELETE /api/v1/mount-sessions/ts1": reply(204, ""),
	}
	for route, h := range routes {
		all[route] = func(w http.ResponseWriter, r *http.Request) {
			data, _ := io.ReadAll(r.Body)
			f.mu.Lock()
			f.requests = append(f.requests, filesRequest{uri: r.RequestURI, header: r.Header.Clone(), body: string(data)})
			f.mu.Unlock()
			h(w, r)
		}
	}
	f.fakeAPI = newFakeAPI(t, all)
	return f
}

func (f *filesAPI) last() filesRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

// checkSession checks that the command created one token session of mode
// with the caller's credentials, used its token for the files call, and
// revoked it at the end.
func (f *filesAPI) checkSession(t *testing.T, mode, auth string) {
	t.Helper()
	calls := f.called()
	if len(calls) != 3 || calls[0] != "POST /api/v1/mount-sessions" || calls[2] != "DELETE /api/v1/mount-sessions/ts1" {
		t.Fatalf("calls = %v", calls)
	}
	body := f.json[0]
	if body["filesystem_id"] != "fsone" || body["mode"] != mode || body["credential"] != "token" ||
		body["max_duration_seconds"] != float64(3600) || body["idempotency_key"] == "" || body["idempotency_key"] == nil {
		t.Errorf("session body = %v", body)
	}
	if f.auth[0] != auth || f.auth[1] != "Bearer "+sessionToken || f.auth[2] != auth {
		t.Errorf("authorization = %v", f.auth)
	}
}

// runFiles runs a files command and checks that the session token stayed
// out of its output.
func runFiles(t *testing.T, args ...string) result {
	t.Helper()
	r := runCLI(t, args...)
	if strings.Contains(r.stdout+r.stderr, "sessiontokensecret") {
		t.Fatalf("%v leaked the session token: %q %q", args, r.stdout, r.stderr)
	}
	return r
}

func withStdin(t *testing.T, data string) {
	t.Helper()
	stdin, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdin
	os.Stdin = stdin
	t.Cleanup(func() { os.Stdin = saved })
	go func() {
		io.WriteString(w, data)
		w.Close()
	}()
}

func TestFilesWriteFromStdin(t *testing.T) {
	useAPIKey(t)
	api := newFilesAPI(t, map[string]http.HandlerFunc{"PUT " + filesPrefix + "/files/notes/a.md": reply(201, written)})
	withStdin(t, "hello")
	r := runFiles(t, "files", "write", "fsone", "/notes/a.md/", "--if-match", "e0", "--json")
	if r.code != 0 || strings.TrimSpace(r.stdout) != written {
		t.Fatalf("files write = %d %q %q", r.code, r.stdout, r.stderr)
	}
	api.checkSession(t, "rw", "Bearer "+testKey)
	req := api.last()
	if req.body != "hello" || req.header.Get("Content-Length") != "5" || req.header.Get("Content-Type") != "application/octet-stream" ||
		req.header.Get("If-Match") != `"e0"` || req.header.Get("If-None-Match") != "" {
		t.Fatalf("request = %+v", req)
	}
}

func TestFilesWriteFromFile(t *testing.T) {
	api := newFilesAPI(t, map[string]http.HandlerFunc{"PUT " + filesPrefix + "/files/notes/a.md": reply(200, written)})
	useLogin(t) // after the fake API: the login is for its URL
	from := filepath.Join(t.TempDir(), "a.md")
	if err := os.WriteFile(from, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := runFiles(t, "files", "write", "fsone", "notes/a.md", "--from", from, "--if-none-match", "--if-match", `W/"e0"`)
	if r.code != 0 || r.stdout != "/notes/a.md e1 5\n" {
		t.Fatalf("files write = %d %q %q", r.code, r.stdout, r.stderr)
	}
	api.checkSession(t, "rw", "Bearer "+testLogin)
	req := api.last()
	if req.body != "hello" || req.header.Get("Content-Length") != "5" || req.header.Get("If-None-Match") != "*" || req.header.Get("If-Match") != `W/"e0"` {
		t.Fatalf("request = %+v", req)
	}

	// An empty file is sent with Content-Length 0.
	empty := filepath.Join(t.TempDir(), "empty")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r = runFiles(t, "files", "write", "fsone", "notes/a.md", "--from", empty)
	if req := api.last(); r.code != 0 || req.body != "" || req.header.Get("Content-Length") != "0" {
		t.Fatalf("empty file = %d %+v", r.code, req)
	}

	r = runFiles(t, "files", "write", "fsone", "notes/a.md", "--from", filepath.Join(t.TempDir(), "missing"))
	if r.code != 1 || !strings.Contains(r.stderr, "missing") {
		t.Fatalf("missing file = %d %q", r.code, r.stderr)
	}
	if calls := api.called(); len(calls) != 6 {
		t.Fatalf("a missing --from file called the API: %v", calls)
	}
}

func fileAnswer(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"e1"`)
		w.Header().Set("Last-Modified", "Thu, 01 Oct 2026 10:00:00 GMT")
		w.Header().Set("Content-Type", "application/octet-stream")
		fmt.Fprint(w, body)
	}
}

func TestFilesRead(t *testing.T) {
	useAPIKey(t)
	api := newFilesAPI(t, map[string]http.HandlerFunc{"GET " + filesPrefix + "/files/notes/a.md": fileAnswer("hello\x00bytes")})
	r := runFiles(t, "files", "read", "fsone", "notes/a.md")
	if r.code != 0 || r.stdout != "hello\x00bytes" || r.stderr != "" {
		t.Fatalf("files read = %d %q %q", r.code, r.stdout, r.stderr)
	}
	api.checkSession(t, "ro", "Bearer "+testKey)

	to := filepath.Join(t.TempDir(), "a.md")
	r = runFiles(t, "files", "read", "fsone", "notes/a.md", "--to", to, "--json")
	want := `{"path":"/notes/a.md","type":"file","size":11,"etag":"\"e1\"","modified_at":"2026-10-01T10:00:00Z"}`
	if r.code != 0 || strings.TrimSpace(r.stdout) != want {
		t.Fatalf("files read --to --json = %d %q %q", r.code, r.stdout, r.stderr)
	}
	if data, err := os.ReadFile(to); err != nil || string(data) != "hello\x00bytes" {
		t.Fatalf("--to wrote %q (%v)", data, err)
	}
	r = runFiles(t, "files", "read", "fsone", "notes/a.md", "--to", to)
	if r.code != 0 || r.stdout != `/notes/a.md "e1" 11`+"\n" {
		t.Fatalf("files read --to = %d %q", r.code, r.stdout)
	}
	if entries, _ := os.ReadDir(filepath.Dir(to)); len(entries) != 1 {
		t.Fatalf("temporary files left: %v", entries)
	}
}

// Without --to, --json is a usage error: the file's bytes are not JSON.
func TestFilesReadJSONNeedsTo(t *testing.T) {
	useAPIKey(t)
	api := newFilesAPI(t, nil)
	r := runFiles(t, "files", "read", "fsone", "notes/a.md", "--json")
	if e := jsonError(t, r); r.code != 2 || e.Code != "usage" || !strings.Contains(e.Message, "--to") {
		t.Fatalf("got %d %+v", r.code, e)
	}
	if calls := api.called(); len(calls) != 0 {
		t.Fatalf("calls = %v", calls)
	}
}

// A body that breaks off fails the command: stdout got only part of it,
// and --to leaves FILE as it was.
func TestFilesReadBrokenOff(t *testing.T) {
	useAPIKey(t)
	api := newFilesAPI(t, map[string]http.HandlerFunc{"GET " + filesPrefix + "/files/big": func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		fmt.Fprint(w, "partial")
	}})
	r := runFiles(t, "files", "read", "fsone", "big")
	if r.code != 1 || r.stdout != "partial" || !strings.Contains(r.stderr, "incomplete") || !strings.Contains(r.stderr, "(network_error)") {
		t.Fatalf("files read = %d %q %q", r.code, r.stdout, r.stderr)
	}
	to := filepath.Join(t.TempDir(), "big")
	if err := os.WriteFile(to, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	r = runFiles(t, "files", "read", "fsone", "big", "--to", to, "--json")
	if e := jsonError(t, r); r.code != 1 || e.Code != "network_error" || !strings.Contains(e.Message, "was not changed") {
		t.Fatalf("files read --to = %d %+v", r.code, e)
	}
	if data, _ := os.ReadFile(to); string(data) != "old" {
		t.Fatalf("FILE was changed: %q", data)
	}
	if entries, _ := os.ReadDir(filepath.Dir(to)); len(entries) != 1 {
		t.Fatalf("temporary files left: %v", entries)
	}
	if calls := api.called(); calls[len(calls)-1] != "DELETE /api/v1/mount-sessions/ts1" {
		t.Fatalf("the session was not revoked: %v", calls)
	}
}

func TestFilesDelete(t *testing.T) {
	useAPIKey(t)
	api := newFilesAPI(t, map[string]http.HandlerFunc{"DELETE " + filesPrefix + "/files/notes/a.md": reply(204, "")})
	r := runFiles(t, "files", "delete", "fsone", "notes/a.md", "--if-match", "e1", "--json")
	if r.code != 0 || strings.TrimSpace(r.stdout) != `{"deleted":true,"path":"/notes/a.md"}` {
		t.Fatalf("files delete --json = %d %q %q", r.code, r.stdout, r.stderr)
	}
	api.checkSession(t, "rw", "Bearer "+testKey)
	if req := api.last(); req.header.Get("If-Match") != `"e1"` {
		t.Fatalf("If-Match = %q", req.header.Get("If-Match"))
	}
	r = runFiles(t, "files", "delete", "fsone", "/notes/a.md")
	if r.code != 0 || r.stdout != "deleted /notes/a.md\n" || api.last().header.Get("If-Match") != "" {
		t.Fatalf("files delete = %d %q", r.code, r.stdout)
	}
}

func TestFilesList(t *testing.T) {
	useAPIKey(t)
	page := `{"entries":[{"path":"/notes","type":"directory","size":0,"modified_at":"2026-10-01T00:00:00Z"},` +
		`{"path":"/notes/a.md","type":"file","size":5,"etag":"e1","modified_at":"2026-10-01T00:00:00Z"}],"next":"a.md"}`
	last := `{"entries":[],"next":null}`
	api := newFilesAPI(t, map[string]http.HandlerFunc{
		"GET " + filesPrefix + "/list/notes": reply(200, page),
		"GET " + filesPrefix + "/list/":      reply(200, last),
	})
	r := runFiles(t, "files", "list", "fsone", "notes", "--after", "a b", "--limit", "2", "--json")
	if r.code != 0 || strings.TrimSpace(r.stdout) != page || r.stderr != "" {
		t.Fatalf("files list --json = %d %q %q", r.code, r.stdout, r.stderr)
	}
	api.checkSession(t, "ro", "Bearer "+testKey)
	if uri := api.last().uri; uri != filesPrefix+"/list/notes?after=a+b&limit=2" {
		t.Fatalf("uri = %q", uri)
	}
	r = runFiles(t, "files", "list", "fsone", "notes")
	want := "TYPE       SIZE  MODIFIED              PATH\n" +
		"directory  0     2026-10-01T00:00:00Z  /notes\n" +
		"file       5     2026-10-01T00:00:00Z  /notes/a.md\n"
	if r.code != 0 || r.stdout != want || !strings.Contains(r.stderr, "--after a.md") {
		t.Fatalf("files list = %d %q %q", r.code, r.stdout, r.stderr)
	}
	if uri := api.last().uri; uri != filesPrefix+"/list/notes" {
		t.Fatalf("uri = %q", uri)
	}
	r = runFiles(t, "files", "list", "fsone")
	if r.code != 0 || r.stdout != "TYPE  SIZE  MODIFIED  PATH\n" || r.stderr != "" {
		t.Fatalf("last page = %d %q %q", r.code, r.stdout, r.stderr)
	}
	for _, args := range [][]string{
		{"files", "list"},
		{"files", "list", "fsone", "a", "b"},
		{"files", "list", "fsone", "--limit", "-1"},
		{"files", "list", "fsone", "--limit", "many"},
	} {
		if r := runFiles(t, args...); r.code != 2 {
			t.Errorf("%v = %d %q", args, r.code, r.stderr)
		}
	}
}

func TestFilesChanges(t *testing.T) {
	useAPIKey(t)
	batch := `{"events":[{"type":"created","path":"/a","file":{"path":"/a","type":"file","size":1,"etag":"e1","modified_at":"2026-10-01T00:00:00Z"}},` +
		`{"type":"deleted","path":"/b","file":null}],"cursor":"42"}`
	api := newFilesAPI(t, map[string]http.HandlerFunc{"GET " + filesPrefix + "/changes": reply(200, batch)})
	r := runFiles(t, "files", "changes", "fsone", "--json")
	if r.code != 0 || strings.TrimSpace(r.stdout) != batch || r.stderr != "" {
		t.Fatalf("files changes --json = %d %q %q", r.code, r.stdout, r.stderr)
	}
	api.checkSession(t, "ro", "Bearer "+testKey)
	if uri := api.last().uri; uri != filesPrefix+"/changes" {
		t.Fatalf("without --cursor, uri = %q", uri)
	}
	r = runFiles(t, "files", "changes", "fsone", "--cursor", "41")
	if r.code != 0 || r.stdout != "created /a\ndeleted /b\n" || !strings.Contains(r.stderr, "--cursor 42") {
		t.Fatalf("files changes = %d %q %q", r.code, r.stdout, r.stderr)
	}
	if uri := api.last().uri; uri != filesPrefix+"/changes?cursor=41&wait=25" {
		t.Fatalf("uri = %q", uri)
	}
	runFiles(t, "files", "changes", "fsone", "--cursor", "41", "--wait", "0")
	if uri := api.last().uri; uri != filesPrefix+"/changes?cursor=41&wait=0" {
		t.Fatalf("uri = %q", uri)
	}
	// Without a cursor, wait is sent only when given.
	runFiles(t, "files", "changes", "fsone", "--wait", "0")
	if uri := api.last().uri; uri != filesPrefix+"/changes?wait=0" {
		t.Fatalf("uri = %q", uri)
	}
	calls := len(api.called())
	for _, args := range [][]string{
		{"files", "changes", "fsone", "--cursor", "41", "--wait", "-1"},
		{"files", "changes"},
	} {
		if r := runFiles(t, args...); r.code != 2 {
			t.Errorf("%v = %d %q", args, r.code, r.stderr)
		}
	}
	if len(api.called()) != calls {
		t.Fatalf("usage errors called the API: %v", api.called())
	}
}

// A change poll may take its wait beyond the usual timeout.
func TestFilesChangesTimeoutIncludesTheWait(t *testing.T) {
	useAPIKey(t)
	saved := httpClient
	httpClient = &http.Client{Timeout: 200 * time.Millisecond}
	t.Cleanup(func() { httpClient = saved })
	slow := func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		reply(200, `{"events":[],"cursor":"42"}`)(w, r)
	}
	newFilesAPI(t, map[string]http.HandlerFunc{"GET " + filesPrefix + "/changes": slow})
	if r := runFiles(t, "files", "changes", "fsone", "--cursor", "41", "--wait", "1", "--json"); r.code != 0 {
		t.Fatalf("with --wait 1 = %d %q", r.code, r.stdout)
	}
	if r := runFiles(t, "files", "changes", "fsone", "--json"); r.code != 1 || jsonError(t, r).Code != "network_error" {
		t.Fatalf("without a wait = %d %q", r.code, r.stdout)
	}
}

// Paths are encoded per segment, as the SDKs do; . and .. are refused
// before any request.
func TestFilesPaths(t *testing.T) {
	useAPIKey(t)
	api := newFilesAPI(t, map[string]http.HandlerFunc{"DELETE " + filesPrefix + "/files/a b/c%#?é!'()*$@+=:;,&.txt": reply(204, "")})
	r := runFiles(t, "files", "delete", "fsone", "//a b/c%#?é!'()*$@+=:;,&.txt/")
	want := filesPrefix + "/files/a%20b/c%25%23%3F%C3%A9%21%27%28%29%2A%24%40%2B%3D%3A%3B%2C%26.txt"
	if r.code != 0 || api.last().uri != want {
		t.Fatalf("got %d %q %q", r.code, api.last().uri, r.stderr)
	}
	calls := len(api.called())
	for _, path := range []string{"a/../b", ".", "a/./b", "..", "/a/.."} {
		r := runFiles(t, "files", "read", "fsone", path, "--to", filepath.Join(t.TempDir(), "x"), "--json")
		if e := jsonError(t, r); r.code != 1 || e.Code != "bad_path" || e.Hint == "" {
			t.Errorf("%q: got %d %+v", path, r.code, e)
		}
	}
	if len(api.called()) != calls {
		t.Fatalf("a bad path called the API: %v", api.called())
	}
}

// precondition_failed carries the file's current ETag (null when nothing
// exists) in the error and its hint; the session is revoked anyway.
func TestFilesPreconditionFailed(t *testing.T) {
	cases := []struct {
		name, answer, etag, hint string
	}{
		{"changed", `{"detail":{"code":"precondition_failed","message":"the file changed","current_etag":"\"e2\""}}`, `"\"e2\""`, `the current ETag is "e2"`},
		{"missing", `{"detail":{"code":"precondition_failed","message":"nothing there","current_etag":null}}`, `null`, "nothing exists"},
		{"top level", `{"code":"precondition_failed","message":"the file changed","current_etag":"e3"}`, `"e3"`, "the current ETag is e3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			useAPIKey(t)
			api := newFilesAPI(t, map[string]http.HandlerFunc{"PUT " + filesPrefix + "/files/a": reply(412, tc.answer)})
			withStdin(t, "x")
			r := runFiles(t, "files", "write", "fsone", "a", "--if-match", "e1", "--json")
			e := jsonError(t, r)
			if r.code != 1 || e.Code != "precondition_failed" || string(e.CurrentETag) != tc.etag || !strings.Contains(e.Hint, tc.hint) || e.Message == "" {
				t.Fatalf("got %d %+v (%s)", r.code, e, e.CurrentETag)
			}
			api.checkSession(t, "rw", "Bearer "+testKey)
			withStdin(t, "x")
			r = runFiles(t, "files", "write", "fsone", "a", "--if-match", "e1")
			if r.code != 1 || !strings.Contains(r.stderr, "hint: "+tc.hint) {
				t.Fatalf("text: %d %q", r.code, r.stderr)
			}
		})
	}
}

// A failed session creation sends no files call; a failed revocation is
// only noted.
func TestFilesSessionErrors(t *testing.T) {
	useAPIKey(t)
	api := newFakeAPI(t, map[string]http.HandlerFunc{"POST /api/v1/mount-sessions": reply(403, `{"detail":{"code":"no_grant"}}`)})
	r := runFiles(t, "files", "read", "fsone", "a")
	if r.code != 1 || r.stdout != "" || !strings.Contains(r.stderr, "(no_grant)") {
		t.Fatalf("got %d %q %q", r.code, r.stdout, r.stderr)
	}
	if calls := api.called(); len(calls) != 1 {
		t.Fatalf("calls = %v", calls)
	}

	files := newFilesAPI(t, map[string]http.HandlerFunc{"GET " + filesPrefix + "/files/a": fileAnswer("hi")})
	files.routes["DELETE /api/v1/mount-sessions/ts1"] = reply(500, "")
	r = runFiles(t, "files", "read", "fsone", "a")
	if r.code != 0 || r.stdout != "hi" || !strings.Contains(r.stderr, "could not revoke token session ts1") {
		t.Fatalf("got %d %q %q", r.code, r.stdout, r.stderr)
	}

	// A missing file's hint points at files list.
	r = runFiles(t, "files", "read", "fsone", "b", "--to", filepath.Join(t.TempDir(), "b"), "--json")
	if e := jsonError(t, r); r.code != 1 || e.Code != "not_found" || !strings.Contains(e.Hint, "mountable files list") {
		t.Fatalf("got %d %+v", r.code, e)
	}
}

func TestFilesUsageErrors(t *testing.T) {
	useAPIKey(t)
	api := newFilesAPI(t, nil)
	for _, args := range [][]string{
		{"files"},
		{"files", "write", "fsone"},
		{"files", "read", "fsone"},
		{"files", "delete", "fsone", "a", "b"},
		{"files", "write", "fsone", "a", "--bogus"},
	} {
		r := runFiles(t, args...)
		if r.code != 2 || r.stdout != "" || !strings.Contains(r.stderr, "usage: mountable files") {
			t.Errorf("%v = %d %q %q", args, r.code, r.stdout, r.stderr)
		}
	}
	if calls := api.called(); len(calls) != 0 {
		t.Fatalf("usage errors called the API: %v", calls)
	}
}

// An --if-match given empty (an unset variable) is a usage error, not an
// unconditional write or delete.
func TestFilesEmptyIfMatch(t *testing.T) {
	useAPIKey(t)
	api := newFilesAPI(t, nil)
	for _, args := range [][]string{
		{"files", "write", "fsone", "a", "--if-match", ""},
		{"files", "write", "fsone", "a", "--if-match="},
		{"files", "delete", "fsone", "a", "--if-match", ""},
	} {
		r := runFiles(t, args...)
		if r.code != 2 || r.stdout != "" || !strings.Contains(r.stderr, "--if-match needs an ETag") {
			t.Errorf("%v = %d %q %q", args, r.code, r.stdout, r.stderr)
		}
		r = runFiles(t, append(args, "--json")...)
		if e := jsonError(t, r); r.code != 2 || e.Code != "usage" {
			t.Errorf("%v --json = %d %+v", args, r.code, e)
		}
	}
	if calls := api.called(); len(calls) != 0 {
		t.Fatalf("an empty --if-match called the API: %v", calls)
	}
}
