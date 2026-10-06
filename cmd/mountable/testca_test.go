package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"testing"
	"time"
)

// testCA issues the certificates a mount session sees: the gateway's server
// certificate and session client certificates.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  string
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key := newKey(t)
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test session CA"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: key, pem: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}

func newKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// issue returns a PEM certificate for public, valid until notAfter.
func (ca *testCA) issue(t *testing.T, name string, public *ecdsa.PublicKey, notAfter time.Time, server bool) string {
	t.Helper()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	if server {
		template.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		template.DNSNames = []string{"localhost"}
		template.IPAddresses = []net.IP{net.IPv4(127, 0, 0, 1)}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, public, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

// sessionCert issues a client certificate for key and pairs it with the key.
func (ca *testCA) sessionCert(t *testing.T, name string, key *ecdsa.PrivateKey, notAfter time.Time) *tls.Certificate {
	t.Helper()
	cert, err := sessionCertificate(key, ca.issue(t, name, &key.PublicKey, notAfter, false))
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// serverTLS is the gateway side: it presents a server certificate and
// requires a client certificate from the CA.
func (ca *testCA) serverTLS(t *testing.T) *tls.Config {
	t.Helper()
	key := newKey(t)
	cert, err := sessionCertificate(key, ca.issue(t, "gateway", &key.PublicKey, time.Now().Add(time.Hour), true))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	return &tls.Config{Certificates: []tls.Certificate{*cert}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}
}
