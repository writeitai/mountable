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

	"github.com/seaweedfs/seaweedfs/weed/operation"
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

// Every TLS path of the engine, gRPC (metadata) and the HTTP clients (chunk
// reads and uploads), presents the current certificate, and the renewed one
// on the next connection.
func TestEveryTLSPathServesTheCurrentCertificate(t *testing.T) {
	ca := newTestCA(t)
	key := newKey(t)
	source := &certSource{}
	source.set(ca.sessionCert(t, "first", key, time.Now().Add(time.Hour)))
	config, err := source.tlsConfig(ca.pem)
	if err != nil {
		t.Fatal(err)
	}

	uploaded := make(chan string, 10)
	web := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.TLS.PeerCertificates[0].Subject.CommonName
		if r.Method == http.MethodPost {
			uploaded <- name
			io.WriteString(w, `{"size": 1}`)
			return
		}
		io.WriteString(w, name)
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

	uploadName := func() string {
		client.CloseIdleConnections()
		uploader, err := operation.NewUploader()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := uploader.UploadData(context.Background(), []byte("x"), &operation.UploadOption{
			UploadUrl: web.URL + "/1,01", MaxAttempts: 1,
		}); err != nil {
			t.Fatal(err)
		}
		return <-uploaded
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
	if got := uploadName(); got != "first" {
		t.Fatalf("the uploader presented %q, want first", got)
	}
	if got := grpcName(); got != "first" {
		t.Fatalf("gRPC presented %q, want first", got)
	}
	source.set(ca.sessionCert(t, "second", key, time.Now().Add(time.Hour)))
	if got := httpName(); got != "second" {
		t.Fatalf("HTTP presented %q after renewal, want second", got)
	}
	if got := uploadName(); got != "second" {
		t.Fatalf("the uploader presented %q after renewal, want second", got)
	}
	if got := grpcName(); got != "second" {
		t.Fatalf("gRPC presented %q after renewal, want second", got)
	}
}
