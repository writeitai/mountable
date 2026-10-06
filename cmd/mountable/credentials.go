package main

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"sync/atomic"
)

// certSource serves the mount session's current client certificate, from
// memory, to every TLS connection the mount opens. Renewal swaps it in place;
// the key and certificate are never written to disk.
type certSource struct {
	current atomic.Pointer[tls.Certificate]
}

func (s *certSource) set(cert *tls.Certificate) { s.current.Store(cert) }

func (s *certSource) get() *tls.Certificate { return s.current.Load() }

func (s *certSource) clientCertificate(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return s.current.Load(), nil
}

// tlsConfig trusts only the session CA for the gateway and presents the
// current certificate. The gateway's name is verified against the address it
// is dialled at.
func (s *certSource) tlsConfig(caPEM string) (*tls.Config, error) {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(caPEM)) {
		return nil, errors.New("the API returned no usable CA certificate")
	}
	return &tls.Config{
		RootCAs:              roots,
		GetClientCertificate: s.clientCertificate,
		MinVersion:           tls.VersionTLS12,
	}, nil
}

// sessionCertificate pairs the PEM certificate chain the API issued with the
// session key.
func sessionCertificate(key *ecdsa.PrivateKey, certPEM string) (*tls.Certificate, error) {
	cert := &tls.Certificate{PrivateKey: key}
	rest := []byte(certPEM)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type == "CERTIFICATE" {
			cert.Certificate = append(cert.Certificate, block.Bytes)
		}
	}
	if len(cert.Certificate) == 0 {
		return nil, errors.New("the API returned no certificate")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, err
	}
	public, ok := leaf.PublicKey.(*ecdsa.PublicKey)
	if !ok || !public.Equal(&key.PublicKey) {
		return nil, errors.New("the API returned a certificate for another key")
	}
	cert.Leaf = leaf
	return cert, nil
}
