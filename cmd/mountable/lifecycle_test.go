package main

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestEndingFor(t *testing.T) {
	cases := map[event]ending{
		eventUnmounted: endFinish,
		eventSignal:    endUnmount,
		eventRevoked:   endAbort,
		eventExpired:   endAbort,
	}
	for ev, want := range cases {
		if got := endingFor(ev); got != want {
			t.Errorf("endingFor(%d) = %d, want %d", ev, got, want)
		}
	}
}

// renewAPI serves the renewal endpoint: refused with refuse, otherwise a new
// certificate for the CSR's key.
func renewAPI(t *testing.T, ca *testCA, refuse int) *int {
	t.Helper()
	calls := new(int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
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
		}
		cert := ca.issue(t, "renewed", csr.PublicKey.(*ecdsa.PublicKey), time.Now().Add(time.Hour), false)
		json.NewEncoder(w).Encode(map[string]any{"certificate": cert})
	}))
	t.Cleanup(server.Close)
	t.Setenv("MOUNTABLE_API_URL", server.URL)
	return calls
}

func testSession(t *testing.T, ca *testCA, notAfter time.Time, probe error) *mountSession {
	key := newKey(t)
	s := &mountSession{id: "s1", key: key, certs: &certSource{}, probe: func() error { return probe }}
	s.certs.set(ca.sessionCert(t, "first", key, notAfter))
	return s
}

func TestCheckExpiredCertificate(t *testing.T) {
	ca := newTestCA(t)
	calls := renewAPI(t, ca, 0)
	s := testSession(t, ca, time.Now().Add(time.Hour), nil)
	if got := s.check(time.Now().Add(2 * time.Hour)); got != eventExpired {
		t.Fatalf("check = %d, want eventExpired", got)
	}
	if *calls != 0 {
		t.Fatal("an expired session called the API")
	}
}

func TestCheckGatewayRejectionConfirmedByAPI(t *testing.T) {
	ca := newTestCA(t)
	renewAPI(t, ca, http.StatusNotFound)
	s := testSession(t, ca, time.Now().Add(time.Hour), status.Error(codes.Unauthenticated, "no active mount session"))
	if got := s.check(time.Now()); got != eventRevoked {
		t.Fatalf("check = %d, want eventRevoked", got)
	}
}

func TestCheckGatewayRejectionTheAPIDoesNotConfirm(t *testing.T) {
	ca := newTestCA(t)
	calls := renewAPI(t, ca, 0)
	s := testSession(t, ca, time.Now().Add(time.Hour), status.Error(codes.Unauthenticated, "database unavailable"))
	if got := s.check(time.Now()); got != 0 {
		t.Fatalf("check = %d, want no event", got)
	}
	if *calls != 1 || s.certs.get().Leaf.Subject.CommonName != "renewed" {
		t.Fatal("the session was not renewed")
	}
}

func TestCheckIgnoresOtherGatewayErrors(t *testing.T) {
	ca := newTestCA(t)
	calls := renewAPI(t, ca, http.StatusNotFound)
	for _, probe := range []error{nil, status.Error(codes.Unavailable, "down"), errors.New("network")} {
		s := testSession(t, ca, time.Now().Add(time.Hour), probe)
		if got := s.check(time.Now()); got != 0 {
			t.Fatalf("check with %v = %d, want no event", probe, got)
		}
	}
	if *calls != 0 {
		t.Fatal("asked the API without a gateway rejection")
	}
}

func TestRenewRefusedMeansRevoked(t *testing.T) {
	ca := newTestCA(t)
	for _, refuse := range []int{http.StatusForbidden, http.StatusNotFound} {
		renewAPI(t, ca, refuse)
		s := testSession(t, ca, time.Now().Add(time.Hour), nil)
		if revoked, _ := s.renew(); !revoked {
			t.Fatalf("a %d renewal is not a revocation", refuse)
		}
	}
	renewAPI(t, ca, http.StatusServiceUnavailable)
	s := testSession(t, ca, time.Now().Add(time.Hour), nil)
	if revoked, err := s.renew(); revoked || err == nil {
		t.Fatal("a 503 renewal must be a retryable error")
	}
}

func TestEngineLogsToStderr(t *testing.T) {
	if err := logToStderr(); err != nil {
		t.Fatal(err)
	}
}
