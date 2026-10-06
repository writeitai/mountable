package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// renewAPI serves the renewal endpoint: refused with refuse, otherwise a new
// certificate for the CSR's key.
func renewAPI(t *testing.T, ca *testCA, refuse int) *atomic.Int32 {
	t.Helper()
	calls := new(atomic.Int32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/v1/mount-sessions/s1:renew" {
			t.Errorf("unexpected %s", r.URL.Path)
		}
		if refuse != 0 {
			w.WriteHeader(refuse)
			json.NewEncoder(w).Encode(map[string]any{"detail": map[string]string{"code": "session_not_active"}})
			return
		}
		var body struct{ CSR string }
		json.NewDecoder(r.Body).Decode(&body)
		block, _ := pem.Decode([]byte(body.CSR))
		csr, err := x509.ParseCertificateRequest(block.Bytes)
		if err != nil {
			t.Error(err)
			return
		}
		cert := ca.issue(t, "renewed", csr.PublicKey.(*ecdsa.PublicKey), time.Now().Add(time.Hour), false)
		json.NewEncoder(w).Encode(map[string]any{"certificate": cert})
	}))
	t.Cleanup(server.Close)
	t.Setenv("MOUNTABLE_API_URL", server.URL)
	return calls
}

func testSession(t *testing.T, ca *testCA, probe func(context.Context) error) *mountSession {
	t.Helper()
	s := newMountSession("s1", newKey(t))
	s.probe = probe
	s.certs.set(ca.sessionCert(t, "first", s.key, time.Now().Add(time.Hour)))
	return s
}

func probeReturning(err error) func(context.Context) error {
	return func(context.Context) error { return err }
}

func TestGatewayRejectionConfirmedByAPI(t *testing.T) {
	ca := newTestCA(t)
	renewAPI(t, ca, http.StatusNotFound)
	s := testSession(t, ca, probeReturning(status.Error(codes.Unauthenticated, "no active mount session")))
	if !s.rejected(context.Background()) {
		t.Fatal("a confirmed rejection was not reported")
	}
}

func TestGatewayRejectionTheAPIDoesNotConfirm(t *testing.T) {
	ca := newTestCA(t)
	calls := renewAPI(t, ca, 0)
	s := testSession(t, ca, probeReturning(status.Error(codes.Unauthenticated, "database unavailable")))
	if s.rejected(context.Background()) {
		t.Fatal("an unconfirmed rejection ended the session")
	}
	if calls.Load() != 1 || s.certs.get().Leaf.Subject.CommonName != "renewed" {
		t.Fatal("the session was not renewed")
	}
	select {
	case <-s.renewed:
	default:
		t.Fatal("the renewal was not announced")
	}
}

func TestOtherGatewayErrorsAreNoRejection(t *testing.T) {
	ca := newTestCA(t)
	calls := renewAPI(t, ca, http.StatusNotFound)
	for _, err := range []error{nil, status.Error(codes.Unavailable, "down"), errors.New("network")} {
		if testSession(t, ca, probeReturning(err)).rejected(context.Background()) {
			t.Fatalf("%v counted as a rejection", err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("asked the API without a gateway rejection")
	}
}

func TestRenewRefusedMeansRevoked(t *testing.T) {
	ca := newTestCA(t)
	for _, refuse := range []int{http.StatusForbidden, http.StatusNotFound} {
		renewAPI(t, ca, refuse)
		if revoked, _ := testSession(t, ca, nil).renew(context.Background()); !revoked {
			t.Fatalf("a %d renewal is not a revocation", refuse)
		}
	}
	renewAPI(t, ca, http.StatusServiceUnavailable)
	if revoked, err := testSession(t, ca, nil).renew(context.Background()); revoked || err == nil {
		t.Fatal("a 503 renewal must be a retryable error")
	}
}

// A gateway that never answers holds the check only until its context ends.
func TestStalledProbeEndsWithItsContext(t *testing.T) {
	ca := newTestCA(t)
	renewAPI(t, ca, 0)
	s := testSession(t, ca, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if s.rejected(ctx) {
		t.Fatal("a stalled probe counted as a rejection")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("the stalled probe outlived its context")
	}
}

func TestWatchStopsWhenTheMountEnds(t *testing.T) {
	ca := newTestCA(t)
	s := testSession(t, ca, probeReturning(nil))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.watch(ctx, make(chan struct{}))
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watch kept running after the mount ended")
	}
}
