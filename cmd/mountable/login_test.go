package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

	if err := login(); err != nil {
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

	if err := login(); err == nil {
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

	err := login()
	if err == nil || strings.Contains(err.Error(), "expired") {
		t.Fatalf("login error = %v, want the decoding error", err)
	}
}
