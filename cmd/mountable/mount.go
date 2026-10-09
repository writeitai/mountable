package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/seaweedfs/seaweedfs/weed/pb"
	"github.com/seaweedfs/seaweedfs/weed/pb/filer_pb"
	"google.golang.org/grpc"
)

type mountCredentials struct {
	SessionID            string `json:"session_id"`
	Certificate          string `json:"certificate"`
	CACertificate        string `json:"ca_certificate"`
	CertificateExpiresIn int    `json:"certificate_expires_in"`
	GatewayAddress       string `json:"gateway_address"`
	Root                 string `json:"root"`
	Mode                 string `json:"mode"`
}

func mountCommand(o *output, args []string) error {
	f := newFlags("mount [--ro] FS_ID DIR | mountable mount --ticket-stdin DIR")
	readOnly := f.Bool("ro", false, "mount read-only")
	ticketStdin := f.Bool("ticket-stdin", false, "read a mount ticket from stdin")
	positional, err := f.parse(args, -1)
	if err != nil {
		return err
	}
	var ticket, dir string
	switch {
	case *ticketStdin && len(positional) == 1:
		dir = positional[0]
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return fmt.Errorf("reading ticket: %w", err)
		}
		ticket = strings.TrimSpace(line)
	case !*ticketStdin && len(positional) == 2:
		dir = positional[1]
		var err error
		if ticket, err = createSession(positional[0], *readOnly); err != nil {
			return err
		}
	default:
		return usageError(f.usage)
	}
	err = mount(o, ticket, dir)
	var api *apiError
	var network *url.Error
	var reported *alreadyReported
	var known *cliError
	if err == nil || errors.As(err, &api) || errors.As(err, &network) || errors.As(err, &reported) || errors.As(err, &known) {
		return err
	}
	return &cliError{Code: "mount_failed", Message: err.Error()}
}

func createSession(filesystemID string, readOnly bool) (string, error) {
	c, err := loginClient("")
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
	err = c.call(context.Background(), "POST", "/api/v1/mount-sessions", map[string]any{
		"filesystem_id": filesystemID, "mode": mode, "idempotency_key": randomHex(16),
	}, &created)
	return created.Ticket, err
}

// mount mounts until the mount ends. With --json it writes the events
// "mounted" and "unmounted" as JSON lines.
func mount(o *output, ticket, dir string) error {
	dir, err := canonicalDir(dir)
	if err != nil {
		return err
	}
	// Check the directory before the ticket is exchanged, so a missing
	// directory does not use up the ticket. Bounded: dir may be a stale mount.
	if err := withTimeout(helperTimeout, func() error { return isDirectory(dir) }); err != nil {
		return &cliError{Code: "mount_failed", Message: fmt.Sprintf("mount directory %s: %v", dir, err), Hint: "create it first: mkdir -p " + dir}
	}
	// Signals are ours from here on, including while the mount starts.
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	// Caches live in a private directory removed on exit.
	cacheDir, err := mountCacheDir()
	if err != nil {
		return err
	}
	defer os.RemoveAll(cacheDir)

	var (
		e       *engine
		session *startedSession
		cleanup = func() {}
	)
	defer func() { cleanup() }()
	interrupted, err := untilSignal(signals, func(ctx context.Context) error {
		var err error
		session, cleanup, err = startSession(ctx, ticket)
		if err != nil {
			return err
		}
		tlsConfig, err := session.tlsConfig()
		if err != nil {
			return err
		}
		e, err = startEngine(ctx, mountConfig{
			gateway:  session.gateway,
			root:     session.root,
			dir:      dir,
			readOnly: session.readOnly,
			tls:      tlsConfig,
			cacheDir: cacheDir,
		})
		return err
	})
	if err != nil {
		if interrupted {
			return errors.New("interrupted while mounting")
		}
		return err
	}
	fmt.Fprintf(o.stderr, "mountable: mounted at %s\n", e.dir)
	if o.json {
		mode := "rw"
		if session.readOnly {
			mode = "ro"
		}
		if err := o.emit(map[string]string{
			"event": "mounted", "path": e.dir, "filesystem_id": path.Base(session.root), "mode": mode,
		}); err != nil {
			fmt.Fprintln(o.stderr, "mountable: writing the mounted event:", err)
		}
	}

	ctx, stopWatching := context.WithCancel(context.Background())
	defer stopWatching() // cancels any outstanding renewal or gateway check
	revoked := make(chan struct{}, 1)
	go session.mountSession.watch(ctx, revoked)
	l := &lifecycle{
		e:        e,
		signals:  signals,
		revoked:  revoked,
		renewed:  session.renewed,
		notAfter: session.notAfter,
		timeout:  shutdownTimeout,
	}
	if interrupted {
		l.ended = eventSignal
		err = l.shutdown(true)
	} else {
		err = l.run()
	}
	if !o.json {
		return err
	}
	detail := ""
	if err != nil {
		detail = redact(err.Error())
	}
	_ = o.emit(map[string]string{"event": "unmounted", "reason": l.endReason(err), "detail": detail})
	if err != nil {
		return &alreadyReported{err}
	}
	return nil
}

// startedSession is a session exchanged for its first certificate, with
// where and how to mount it.
type startedSession struct {
	*mountSession
	caPEM    string
	gateway  string
	root     string
	readOnly bool
}

func (s *startedSession) tlsConfig() (*tls.Config, error) {
	return s.certs.tlsConfig(s.caPEM)
}

// startSession exchanges the ticket for the session's first certificate and
// prepares the gateway check. cleanup releases the check's connection.
func startSession(ctx context.Context, ticket string) (*startedSession, func(), error) {
	noop := func() {}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, noop, err
	}
	csr, err := certificateRequest(key)
	if err != nil {
		return nil, noop, err
	}
	var creds mountCredentials
	if err := callContext(ctx, "POST", "/api/v1/mount-sessions:exchange", "", map[string]string{
		"ticket": ticket, "csr": csr,
	}, &creds); err != nil {
		return nil, noop, err
	}
	cert, err := sessionCertificate(key, creds.Certificate)
	if err != nil {
		return nil, noop, err
	}
	session := &startedSession{
		mountSession: newMountSession(creds.SessionID, key),
		caPEM:        creds.CACertificate,
		gateway:      creds.GatewayAddress,
		root:         creds.Root,
		readOnly:     creds.Mode == "ro",
	}
	session.certs.set(cert)
	tlsConfig, err := session.tlsConfig()
	if err != nil {
		return nil, noop, err
	}
	conn, err := grpc.NewClient(pb.ServerAddress(creds.GatewayAddress).ToGrpcAddress(), grpcDialOption(tlsConfig))
	if err != nil {
		return nil, noop, err
	}
	session.probe = func(ctx context.Context) error {
		_, err := filer_pb.NewSeaweedFilerClient(conn).GetFilerConfiguration(ctx, &filer_pb.GetFilerConfigurationRequest{})
		return err
	}
	return session, func() { conn.Close() }, nil
}

// untilSignal runs fn, cancelling its context if a signal arrives first;
// interrupted reports whether one did.
func untilSignal(signals <-chan os.Signal, fn func(ctx context.Context) error) (interrupted bool, err error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- fn(ctx) }()
	select {
	case err := <-result:
		return false, err
	case <-signals:
		cancel()
		return true, <-result
	}
}

func isDirectory(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("not a directory")
	}
	return nil
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
// mounting process finishes its pending writes and exits. A mount that
// cannot be unmounted cleanly (busy, or not answering) is aborted instead.
func unmount(dir string) error {
	dir, err := canonicalDir(dir)
	if err != nil {
		return err
	}
	err = cleanUnmount(dir)
	if err == nil {
		return nil
	}
	why := fmt.Sprintf("%s could not be unmounted cleanly (%s)", dir, oneLine(err))
	fmt.Fprintf(os.Stderr, "mountable: %s; aborting the mount\n", why)
	if err := abortMount(dir); err != nil {
		return fmt.Errorf("%s, and the mount could not be fully aborted (%s); writes not yet committed may be lost", why, oneLine(err))
	}
	return fmt.Errorf("%s; the mount was aborted and writes not yet committed may be lost", why)
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
