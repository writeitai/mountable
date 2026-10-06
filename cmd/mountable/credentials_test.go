package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	util_http "github.com/seaweedfs/seaweedfs/weed/util/http"
	"google.golang.org/grpc"
	grpccredentials "google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/peer"
)

func TestCertSourceSwapsInPlace(t *testing.T) {
	ca := newTestCA(t)
	key := newKey(t)
	first := ca.sessionCert(t, "first", key, time.Now().Add(time.Hour))
	second := ca.sessionCert(t, "second", key, time.Now().Add(time.Hour))
	source := &certSource{}
	source.set(first)
	if got, _ := source.clientCertificate(nil); got != first {
		t.Fatal("not serving the first certificate")
	}
	source.set(second)
	if got, _ := source.clientCertificate(nil); got != second {
		t.Fatal("renewal did not swap the certificate")
	}
}

func TestSessionCertificateRejectsAnotherKey(t *testing.T) {
	ca := newTestCA(t)
	other := newKey(t)
	if _, err := sessionCertificate(newKey(t), ca.issue(t, "x", &other.PublicKey, time.Now().Add(time.Hour), false)); err == nil {
		t.Fatal("accepted a certificate for another key")
	}
	if _, err := sessionCertificate(newKey(t), "not a certificate"); err == nil {
		t.Fatal("accepted no certificate")
	}
}

func TestTLSConfigRejectsMissingCA(t *testing.T) {
	if _, err := (&certSource{}).tlsConfig(""); err == nil {
		t.Fatal("accepted an empty CA")
	}
}

// Both of the engine's TLS paths, gRPC (metadata) and the process-wide HTTP
// client (chunks), present the current certificate, and the renewed one on
// the next connection.
func TestBothTLSPathsServeTheCurrentCertificate(t *testing.T) {
	ca := newTestCA(t)
	key := newKey(t)
	source := &certSource{}
	source.set(ca.sessionCert(t, "first", key, time.Now().Add(time.Hour)))
	config, err := source.tlsConfig(ca.pem)
	if err != nil {
		t.Fatal(err)
	}

	web := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, r.TLS.PeerCertificates[0].Subject.CommonName)
	}))
	web.TLS = ca.serverTLS(t)
	web.StartTLS()
	defer web.Close()
	if err := useHTTPClientTLS(config); err != nil {
		t.Fatal(err)
	}
	client := util_http.GetGlobalHttpClient()
	httpName := func() string {
		client.CloseIdleConnections()
		// The engine passes host:port; the client adds the scheme.
		resp, err := client.Get(strings.TrimPrefix(web.URL, "https://"))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	seen := make(chan string, 1)
	server := grpc.NewServer(grpc.Creds(grpccredentials.NewTLS(ca.serverTLS(t))),
		grpc.UnaryInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			p, _ := peer.FromContext(ctx)
			seen <- p.AuthInfo.(grpccredentials.TLSInfo).State.PeerCertificates[0].Subject.CommonName
			return handler(ctx, req)
		}))
	healthpb.RegisterHealthServer(server, health.NewServer())
	go server.Serve(listener)
	defer server.Stop()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	grpcName := func() string {
		conn, err := grpc.NewClient("localhost:"+port, grpcDialOption(config))
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		if _, err := healthpb.NewHealthClient(conn).Check(context.Background(), &healthpb.HealthCheckRequest{}); err != nil {
			t.Fatal(err)
		}
		return <-seen
	}

	if got := httpName(); got != "first" {
		t.Fatalf("HTTP presented %q, want first", got)
	}
	if got := grpcName(); got != "first" {
		t.Fatalf("gRPC presented %q, want first", got)
	}
	source.set(ca.sessionCert(t, "second", key, time.Now().Add(time.Hour)))
	if got := httpName(); got != "second" {
		t.Fatalf("HTTP presented %q after renewal, want second", got)
	}
	if got := grpcName(); got != "second" {
		t.Fatalf("gRPC presented %q after renewal, want second", got)
	}
}
