package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// How often the certificate is renewed; a refused renewal means the session
// was revoked, and the mount is aborted.
const renewEvery = 2 * time.Minute

// How often the gateway is asked whether it still accepts the session, and
// the certificate's expiry is checked.
const checkEvery = 5 * time.Second

type mountCredentials struct {
	SessionID            string `json:"session_id"`
	Certificate          string `json:"certificate"`
	CACertificate        string `json:"ca_certificate"`
	CertificateExpiresIn int    `json:"certificate_expires_in"`
	GatewayAddress       string `json:"gateway_address"`
	Root                 string `json:"root"`
	Mode                 string `json:"mode"`
}

func mountCommand(args []string) error {
	flags := flag.NewFlagSet("mount", flag.ContinueOnError)
	readOnly := flags.Bool("ro", false, "mount read-only")
	ticketStdin := flags.Bool("ticket-stdin", false, "read a mount ticket from stdin")
	if err := flags.Parse(args); err != nil {
		return err
	}
	var ticket, dir string
	switch {
	case *ticketStdin && flags.NArg() == 1:
		dir = flags.Arg(0)
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return fmt.Errorf("reading ticket: %w", err)
		}
		ticket = strings.TrimSpace(line)
	case !*ticketStdin && flags.NArg() == 2:
		dir = flags.Arg(1)
		var err error
		if ticket, err = createSession(flags.Arg(0), *readOnly); err != nil {
			return err
		}
	default:
		return errors.New("usage: mountable mount [--ro] FILESYSTEM_ID DIR | mountable mount --ticket-stdin DIR")
	}
	return mount(ticket, dir)
}

func createSession(filesystemID string, readOnly bool) (string, error) {
	token, err := accessToken()
	if err != nil {
		return "", err
	}
	mode := "rw"
	if readOnly {
		mode = "ro"
	}
	var created struct {
		Ticket string `json:"ticket"`
	}
	err = call("POST", "/api/v1/mount-sessions", token, map[string]any{
		"filesystem_id": filesystemID, "mode": mode, "idempotency_key": randomHex(16),
	}, &created)
	return created.Ticket, err
}

// mountSession is the live session behind a mount: its key, its current
// certificate, and a way to ask the gateway whether it still accepts it.
type mountSession struct {
	id    string
	key   *ecdsa.PrivateKey
	certs *certSource
	probe func() error
}

func mount(ticket, dir string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	var creds mountCredentials
	csr, err := certificateRequest(key)
	if err != nil {
		return err
	}
	if err := call("POST", "/api/v1/mount-sessions:exchange", "", map[string]string{
		"ticket": ticket, "csr": csr,
	}, &creds); err != nil {
		return err
	}
	cert, err := sessionCertificate(key, creds.Certificate)
	if err != nil {
		return err
	}
	session := &mountSession{id: creds.SessionID, key: key, certs: &certSource{}}
	session.certs.set(cert)
	tlsConfig, err := session.certs.tlsConfig(creds.CACertificate)
	if err != nil {
		return err
	}

	// Caches live in a private directory removed on exit.
	cacheDir, err := mountCacheDir()
	if err != nil {
		return err
	}
	defer os.RemoveAll(cacheDir)

	probeConn, err := grpc.NewClient(pb.ServerAddress(creds.GatewayAddress).ToGrpcAddress(), grpcDialOption(tlsConfig))
	if err != nil {
		return err
	}
	defer probeConn.Close()
	session.probe = func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := filer_pb.NewSeaweedFilerClient(probeConn).GetFilerConfiguration(ctx, &filer_pb.GetFilerConfigurationRequest{})
		return err
	}

	e, err := startEngine(mountConfig{
		gateway:  creds.GatewayAddress,
		root:     creds.Root,
		dir:      dir,
		readOnly: creds.Mode == "ro",
		tls:      tlsConfig,
		cacheDir: cacheDir,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "mountable: mounted at %s\n", e.dir)
	return serve(e, session)
}

// serve keeps the session alive until something ends the mount, then ends
// it the way that event requires.
func serve(e *engine, session *mountSession) error {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	renewal := time.NewTicker(renewEvery)
	defer renewal.Stop()
	check := time.NewTicker(checkEvery)
	defer check.Stop()
	for {
		var ev event
		select {
		case <-e.served:
			ev = eventUnmounted
		case <-signals:
			ev = eventSignal
		case <-renewal.C:
			revoked, err := session.renew()
			if revoked {
				ev = eventRevoked
			} else if err != nil {
				fmt.Fprintln(os.Stderr, "mountable: renewal failed, retrying:", err)
			}
		case <-check.C:
			ev = session.check(time.Now())
		}
		if ev != 0 {
			return end(e, ev)
		}
	}
}

func end(e *engine, ev event) error {
	switch endingFor(ev) {
	case endFinish:
		e.finish()
		return nil
	case endUnmount:
		err := e.unmount()
		if err == nil {
			e.finish()
			return nil
		}
		fmt.Fprintf(os.Stderr, "mountable: %s could not be unmounted cleanly (%s); aborting the mount\n", e.dir, oneLine(err))
		e.abort()
		return errors.New("the mount was aborted; writes not yet committed may be lost")
	default:
		reason := "was revoked"
		if ev == eventExpired {
			reason = "expired"
		}
		fmt.Fprintf(os.Stderr, "mountable: the mount session %s; aborting the mount\n", reason)
		e.abort()
		return fmt.Errorf("the mount session %s; the mount was aborted and writes not yet committed may be lost", reason)
	}
}

// check reports eventExpired once the certificate has run out, and
// eventRevoked once the gateway refuses the session and the API, the
// authority, confirms it. Otherwise it returns 0.
func (s *mountSession) check(now time.Time) event {
	if !now.Before(s.certs.get().Leaf.NotAfter) {
		return eventExpired
	}
	if status.Code(s.probe()) != codes.Unauthenticated {
		return 0
	}
	if revoked, _ := s.renew(); revoked {
		return eventRevoked
	}
	return 0
}

// renew gets a new certificate for the same key and serves it from then on.
// revoked is true when the API refused: the session no longer exists or lost
// its authority.
func (s *mountSession) renew() (revoked bool, err error) {
	csr, err := certificateRequest(s.key)
	if err != nil {
		return false, err
	}
	var creds mountCredentials
	err = call("POST", "/api/v1/mount-sessions/"+s.id+":renew", "", map[string]string{"csr": csr}, &creds)
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
	return false, nil
}

// mountCacheDir creates a private cache directory for one mount.
func mountCacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	base = filepath.Join(base, "mountable")
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	return os.MkdirTemp(base, "mount-")
}

// unmount is `mountable unmount DIR`: a clean unmount, after which the
// mounting process finishes its pending writes and exits. A busy mount is
// aborted instead.
func unmount(dir string) error {
	err := cleanUnmount(dir)
	if err == nil {
		return nil
	}
	fmt.Fprintf(os.Stderr, "mountable: %s could not be unmounted cleanly (%s); aborting the mount\n", dir, oneLine(err))
	if err := abortConnection(dir); err != nil {
		fmt.Fprintln(os.Stderr, "mountable: aborting the FUSE connection:", err)
	}
	if err := detach(dir); err != nil {
		return err
	}
	return errors.New("the mount was aborted; writes not yet committed may be lost")
}

// oneLine flattens an error from an unmount helper, which may span lines.
func oneLine(err error) string {
	return strings.Join(strings.Fields(err.Error()), " ")
}

func certificateRequest(key *ecdsa.PrivateKey) (string, error) {
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "mountable mount"},
	}, key)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})), nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
