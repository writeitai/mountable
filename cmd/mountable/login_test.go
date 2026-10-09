package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// The first poll's connection is dropped mid-request, the second is still
// pending, the third is approved: login must ride out the dropped poll.
func TestLoginRetriesDroppedPoll(t *testing.T) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/device/code":
			fmt.Fprint(w, `{"device_code":"d","user_code":"ABCD-EFGH","verification_uri_complete":"x","expires_in":60,"interval":0}`)
		case "/auth/device/token":
			switch polls.Add(1) {
			case 1:
				conn, _, _ := w.(http.Hijacker).Hijack()
				conn.Close()
			case 2:
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":"authorization_pending"}`)
			default:
				fmt.Fprint(w, `{"access_token":"a","refresh_token":"r","expires_in":3600}`)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("MOUNTABLE_API_URL", srv.URL)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if err := login(io.Discard); err != nil {
		t.Fatalf("login: %v", err)
	}
	if n := polls.Load(); n != 3 {
		t.Fatalf("polls = %d, want 3", n)
	}
}

// A real API error ends the login instead of polling until expiry.
func TestLoginStopsOnAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/device/code" {
			fmt.Fprint(w, `{"device_code":"d","user_code":"ABCD-EFGH","verification_uri_complete":"x","expires_in":60,"interval":0}`)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"access_denied"}`)
	}))
	defer srv.Close()
	t.Setenv("MOUNTABLE_API_URL", srv.URL)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	if err := login(io.Discard); err == nil {
		t.Fatal("login succeeded after access_denied")
	}
}

// A malformed success response is reported, not retried until expiry.
func TestLoginReportsMalformedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/auth/device/code" {
			fmt.Fprint(w, `{"device_code":"d","user_code":"ABCD-EFGH","verification_uri_complete":"x","expires_in":60,"interval":0}`)
			return
		}
		fmt.Fprint(w, `{"access_token":123}`)
	}))
	defer srv.Close()
	t.Setenv("MOUNTABLE_API_URL", srv.URL)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	err := login(io.Discard)
	if err == nil || strings.Contains(err.Error(), "expired") {
		t.Fatalf("login error = %v, want the decoding error", err)
	}
}

// refreshingAPI serves the organisation's filesystems to the refreshed
// token only, and rotates the login on refresh unless refreshStatus says
// otherwise. It reports the refresh token each refresh was sent.
func refreshingAPI(t *testing.T, refreshStatus int) (*fakeAPI, *[]string) {
	var mu sync.Mutex
	var refreshed []string
	filesystems := func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testJWT {
			reply(401, `{"detail":{"code":"unauthenticated"}}`)(w, r)
			return
		}
		if r.Method == http.MethodPost {
			reply(201, `{"id":"fsnew"}`)(w, r)
			return
		}
		reply(200, `[{"id":"fsone"}]`)(w, r)
	}
	api := newFakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/v1/orgs/" + orgA + "/filesystems":  filesystems,
		"POST /api/v1/orgs/" + orgA + "/filesystems": filesystems,
		"POST /auth/device/token": func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			mu.Lock()
			refreshed = append(refreshed, r.PostForm.Get("grant_type")+" "+r.PostForm.Get("refresh_token"))
			mu.Unlock()
			if refreshStatus != 200 {
				reply(refreshStatus, `{"error":"invalid_grant"}`)(w, r)
				return
			}
			reply(200, `{"access_token":"`+testJWT+`","refresh_token":"mtblrt_rotated","expires_in":900}`)(w, r)
		},
	})
	t.Setenv("MOUNTABLE_ORG", orgA)
	return api, &refreshed
}

// A login refused before its expiry is refreshed once, the rotated pair is
// saved, and the request, body included, is sent again.
func TestRefusedLoginIsRefreshedAndRetried(t *testing.T) {
	api, refreshed := refreshingAPI(t, 200)
	useLogin(t)
	r := runCLI(t, "fs", "create", "x", "--json")
	if r.code != 0 || !strings.Contains(r.stdout, `"fsnew"`) {
		t.Fatalf("got %d %q %q", r.code, r.stdout, r.stderr)
	}
	want := []string{"POST /api/v1/orgs/" + orgA + "/filesystems", "POST /auth/device/token", "POST /api/v1/orgs/" + orgA + "/filesystems"}
	if calls := api.called(); !slices.Equal(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
	if got := *refreshed; !slices.Equal(got, []string{"refresh_token mtblrt_refreshsecret"}) {
		t.Fatalf("refreshes = %v", got)
	}
	if api.bodies[0]["name"] != "x" || api.bodies[2]["name"] != "x" {
		t.Fatalf("bodies = %v", api.bodies)
	}
	c, err := loadCredentials()
	if err != nil || c.AccessToken != testJWT || c.RefreshToken != "mtblrt_rotated" || c.Generation != testGeneration {
		t.Fatalf("saved %+v, %v", c, err)
	}
	// The next command uses the saved login without refreshing.
	if r := runCLI(t, "fs", "list", "--json"); r.code != 0 || len(*refreshed) != 1 {
		t.Fatalf("next command: %d %q, refreshes %v", r.code, r.stdout, *refreshed)
	}
}

// When the refresh fails, the API's refusal is reported with its hint, and
// the request is not sent again.
func TestRefusedLoginWithFailedRefresh(t *testing.T) {
	api, _ := refreshingAPI(t, 400)
	useLogin(t)
	r := runCLI(t, "fs", "list", "--json")
	if e := jsonError(t, r); r.code != 1 || e.Code != "unauthenticated" || !strings.Contains(e.Hint, "mountable login") {
		t.Fatalf("got %d %+v", r.code, e)
	}
	want := []string{"GET /api/v1/orgs/" + orgA + "/filesystems", "POST /auth/device/token"}
	if calls := api.called(); !slices.Equal(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

// A request refused again after a refresh is not retried a second time.
func TestRefusedLoginIsRetriedOnlyOnce(t *testing.T) {
	api := newFakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/v1/orgs/" + orgA + "/filesystems": reply(401, `{"detail":{"code":"unauthenticated"}}`),
		"POST /auth/device/token":                   reply(200, `{"access_token":"`+testJWT+`","refresh_token":"mtblrt_rotated","expires_in":900}`),
	})
	t.Setenv("MOUNTABLE_ORG", orgA)
	useLogin(t)
	r := runCLI(t, "fs", "list", "--json")
	if e := jsonError(t, r); r.code != 1 || e.Code != "unauthenticated" {
		t.Fatalf("got %d %+v", r.code, e)
	}
	want := []string{"GET /api/v1/orgs/" + orgA + "/filesystems", "POST /auth/device/token", "GET /api/v1/orgs/" + orgA + "/filesystems"}
	if calls := api.called(); !slices.Equal(calls, want) {
		t.Fatalf("calls = %v, want %v", calls, want)
	}
}

// An API key refused by the API is never refreshed or retried.
func TestRefusedAPIKeyIsNotRetried(t *testing.T) {
	api, refreshed := refreshingAPI(t, 200)
	useAPIKey(t)
	r := runCLI(t, "fs", "list", "--json")
	if e := jsonError(t, r); r.code != 1 || e.Code != "unauthenticated" {
		t.Fatalf("got %d %+v", r.code, e)
	}
	if calls := api.called(); len(calls) != 1 || len(*refreshed) != 0 {
		t.Fatalf("calls = %v, refreshes = %v", calls, *refreshed)
	}
}

// The MCP server's tools refresh a refused login the same way.
func TestMCPRefreshesRefusedLogin(t *testing.T) {
	api, refreshed := refreshingAPI(t, 200)
	useLogin(t)
	s := connectMCP(t)
	text, isError := callTool(t, s, "list_filesystems", nil)
	if isError || !strings.Contains(text, "fsone") || len(*refreshed) != 1 {
		t.Fatalf("result = %v %s, calls %v", isError, text, api.called())
	}
}

// rotatingTokenAPI redeems each refresh token once, as the API does, and
// counts the refreshes.
func rotatingTokenAPI(t *testing.T) *atomic.Int32 {
	var mu sync.Mutex
	used := map[string]bool{}
	refreshes := new(atomic.Int32)
	newFakeAPI(t, map[string]http.HandlerFunc{
		"POST /auth/device/token": func(w http.ResponseWriter, r *http.Request) {
			_ = r.ParseForm()
			token := r.PostForm.Get("refresh_token")
			mu.Lock()
			reused := used[token]
			used[token] = true
			mu.Unlock()
			if reused {
				reply(400, `{"error":"invalid_grant"}`)(w, r)
				return
			}
			refreshes.Add(1)
			reply(200, `{"access_token":"`+testJWT+`","refresh_token":"mtblrt_rotated","expires_in":900}`)(w, r)
		},
	})
	return refreshes
}

// Simultaneous callers (MCP tool calls, or processes) redeem the
// single-use refresh token once and all end up with the new access token,
// whether the refresh is due to expiry or to a refusal.
func TestConcurrentRefreshRedeemsOnce(t *testing.T) {
	for _, tc := range []struct {
		name      string
		expiresIn int
		get       func() (string, error)
	}{
		{"expiry", 0, func() (string, error) { token, _, err := accessToken(); return token, err }},
		{"refusal", 3600, func() (string, error) { return refreshRejected(testLogin, testGeneration) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refreshes := rotatingTokenAPI(t)
			isolateConfig(t)
			if err := saveCredentials(tokenResponse{AccessToken: testLogin, RefreshToken: "mtblrt_refreshsecret", ExpiresIn: tc.expiresIn}, testGeneration); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			tokens := make([]string, 8)
			errs := make([]error, 8)
			for i := range tokens {
				wg.Go(func() { tokens[i], errs[i] = tc.get() })
			}
			wg.Wait()
			for i := range tokens {
				if errs[i] != nil || tokens[i] != testJWT {
					t.Fatalf("caller %d: %q, %v", i, tokens[i], errs[i])
				}
			}
			if n := refreshes.Load(); n != 1 {
				t.Fatalf("refreshes = %d, want 1", n)
			}
		})
	}
}

// The credentials lock is a file lock, so another process holding it
// blocks this one.
func TestCredentialsLockIsAFileLock(t *testing.T) {
	isolateConfig(t)
	t.Setenv("MOUNTABLE_API_URL", "http://127.0.0.1:1")
	path, err := credentialsPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	other, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err := unix.Flock(int(other.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- saveCredentials(tokenResponse{AccessToken: testLogin, RefreshToken: "mtblrt_refreshsecret", ExpiresIn: 3600}, testGeneration)
	}()
	select {
	case <-done:
		t.Fatal("saved while another holder had the lock")
	case <-time.After(200 * time.Millisecond):
	}
	if err := unix.Flock(int(other.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("still blocked after the lock was released")
	}
}

// Saving replaces the file with an owner-only one, even over a file with
// wider permissions, and leaves no temporary file behind.
func TestCredentialsAreReplacedOwnerOnly(t *testing.T) {
	isolateConfig(t)
	t.Setenv("MOUNTABLE_API_URL", "http://127.0.0.1:1")
	path, err := credentialsPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveCredentials(tokenResponse{AccessToken: testJWT, RefreshToken: "mtblrt_rotated", ExpiresIn: 900}, testGeneration); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v", info.Mode(), err)
	}
	if c, err := loadCredentials(); err != nil || c.AccessToken != testJWT {
		t.Fatalf("loaded %+v, %v", c, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !slices.Equal(names, []string{"credentials.json", "credentials.json.lock"}) {
		t.Fatalf("files = %v", names)
	}
}

// A new `mountable login` saved while a request was in flight is not used
// to retry that request: it may be another account.
func TestRefusedRequestIsNotRetriedUnderANewLogin(t *testing.T) {
	api := newFakeAPI(t, map[string]http.HandlerFunc{
		"GET /api/v1/orgs/" + orgA + "/filesystems": func(w http.ResponseWriter, r *http.Request) {
			if err := saveCredentials(tokenResponse{AccessToken: testJWT, RefreshToken: "mtblrt_rotated", ExpiresIn: 900}, "gen-2"); err != nil {
				t.Error(err)
			}
			reply(401, `{"detail":{"code":"unauthenticated"}}`)(w, r)
		},
	})
	t.Setenv("MOUNTABLE_ORG", orgA)
	useLogin(t)
	r := runCLI(t, "fs", "list", "--json")
	if e := jsonError(t, r); r.code != 1 || e.Code != "unauthenticated" || !strings.Contains(e.Hint, "mountable login") {
		t.Fatalf("got %d %+v", r.code, e)
	}
	if calls := api.called(); len(calls) != 1 || api.auth[0] != "Bearer "+testLogin {
		t.Fatalf("calls = %v %v", calls, api.auth)
	}
}

// A login saved while logout's sign-out was in flight survives the logout.
func TestLogoutKeepsALoginSavedMeanwhile(t *testing.T) {
	newFakeAPI(t, map[string]http.HandlerFunc{
		"POST /auth/cli/sign-out": func(w http.ResponseWriter, r *http.Request) {
			if err := saveCredentials(tokenResponse{AccessToken: testJWT, RefreshToken: "mtblrt_rotated", ExpiresIn: 900}, "gen-2"); err != nil {
				t.Error(err)
			}
			reply(204, "")(w, r)
		},
	})
	useLogin(t)
	if r := runCLI(t, "logout", "--json"); r.code != 0 {
		t.Fatalf("logout = %d %q %q", r.code, r.stdout, r.stderr)
	}
	if c, err := loadCredentials(); err != nil || c.Generation != "gen-2" || c.AccessToken != testJWT {
		t.Fatalf("the new login was removed: %+v, %v", c, err)
	}
}
