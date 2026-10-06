package main

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"os"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// How often the certificate is renewed; a refused renewal means the session
// was revoked, and the mount is aborted.
const renewEvery = 2 * time.Minute

// How often the gateway is asked whether it still accepts the session.
const checkEvery = 5 * time.Second

// requestTimeout bounds each call to the gateway or the API.
const requestTimeout = 10 * time.Second

// mountSession is the live session behind a mount: its key, its current
// certificate, and a way to ask the gateway whether it still accepts it.
type mountSession struct {
	id    string
	key   *ecdsa.PrivateKey
	certs *certSource
	probe func(ctx context.Context) error
	// renewed receives a value (without blocking) whenever the certificate
	// was replaced, so the lifecycle can re-arm its expiry timer.
	renewed chan struct{}
}

func newMountSession(id string, key *ecdsa.PrivateKey) *mountSession {
	return &mountSession{id: id, key: key, certs: &certSource{}, renewed: make(chan struct{}, 1)}
}

// notAfter is when the current certificate, and with it the mount, expires.
func (s *mountSession) notAfter() time.Time { return s.certs.get().Leaf.NotAfter }

// watch is the mount's only network worker: it renews the certificate and
// checks with the gateway, each call bounded by requestTimeout, until ctx
// ends. It sends on revoked once the API refuses the session, then returns.
// The lifecycle never waits on it.
func (s *mountSession) watch(ctx context.Context, revoked chan<- struct{}) {
	renewal := time.NewTicker(renewEvery)
	defer renewal.Stop()
	check := time.NewTicker(checkEvery)
	defer check.Stop()
	for {
		var gone bool
		select {
		case <-ctx.Done():
			return
		case <-renewal.C:
			var err error
			gone, err = s.renew(ctx)
			if !gone && err != nil && ctx.Err() == nil {
				fmt.Fprintln(os.Stderr, "mountable: renewal failed, retrying:", err)
			}
		case <-check.C:
			gone = s.rejected(ctx)
		}
		if gone {
			select {
			case revoked <- struct{}{}:
			case <-ctx.Done():
			}
			return
		}
	}
}

// rejected reports whether the gateway refuses the session and the API, the
// authority, confirms it: a gateway rejection alone (say, its database is
// down) never ends a mount.
func (s *mountSession) rejected(ctx context.Context) bool {
	probeCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	err := s.probe(probeCtx)
	cancel()
	if status.Code(err) != codes.Unauthenticated {
		return false
	}
	revoked, _ := s.renew(ctx)
	return revoked
}

// renew gets a new certificate for the same key and serves it from then on.
// revoked is true when the API refused: the session no longer exists or lost
// its authority.
func (s *mountSession) renew(ctx context.Context) (revoked bool, err error) {
	csr, err := certificateRequest(s.key)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var creds mountCredentials
	err = callContext(ctx, "POST", "/api/v1/mount-sessions/"+s.id+":renew", "", map[string]string{"csr": csr}, &creds)
	var api *apiError
	if errors.As(err, &api) && (api.Status == 403 || api.Status == 404) {
		return true, err
	}
	if err != nil {
		return false, err
	}
	cert, err := sessionCertificate(s.key, creds.Certificate)
	if err != nil {
		return false, err
	}
	s.certs.set(cert)
	select {
	case s.renewed <- struct{}{}:
	default:
	}
	return false, nil
}
