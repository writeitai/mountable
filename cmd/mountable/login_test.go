package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
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
	if err != nil || c.AccessToken != testJWT || c.RefreshToken != "mtblrt_rotated" {
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
