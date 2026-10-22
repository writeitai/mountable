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
	"golang.org/x/sys/unix"
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
	defer flushEngineLog()
	dir, err := canonicalDir(dir)
	if err != nil {
		return err
	}
	// Check the directory before the ticket is exchanged, so a missing
	// directory does not use up the ticket. Bounded: dir may be a stale mount.
	if err := prepareMountPoint(dir); err != nil {
		return err
	}
	if err := withTimeout(helperTimeout, func() error { return isDirectory(dir) }); err != nil {
		return &cliError{Code: "mount_failed", Message: fmt.Sprintf("mount directory %s: %v", dir, err), Hint: "create it first: mkdir -p " + dir}
	}
	// Signals are ours from here on, including while the mount starts.
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	// Caches live in a private directory removed on exit.
	cacheDir, release, err := mountCacheDir()
	if err != nil {
		return err
	}
	defer release()
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
	defer e.close() // however the mount ends, also when its shutdown was cut short
	note(o.stderr, "mounted at %s", e.dir)
	if o.json {
		mode := "rw"
		if e.readOnly {
			mode = "ro"
		}
		if err := o.emit(map[string]string{
			"event": "mounted", "path": e.dir, "filesystem_id": path.Base(session.root), "mode": mode,
		}); err != nil {
			note(o.stderr, "writing the mounted event: %v", err)
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

// mountCacheDir creates a private cache directory for one mount, and
// returns it with its lock, held until release. Without a user cache
// directory it goes under the temporary directory, in a directory of this
// user's own. The directory it goes in must be this user's, mode 0700 and
// not a symlink. The directories this host's mounts left when their
// process ended without removing them (killed or crashed) are removed
// first.
//
// Creating and pruning hold the root's lock, so no mount prunes a
// directory another is still creating and locking. A filesystem without
// flock (some network homes) keeps working: nothing is locked there, and
// nothing is pruned.
func mountCacheDir() (string, func(), error) {
	base, err := os.UserCacheDir()
	if err == nil {
		base = filepath.Join(base, "mountable")
	} else {
		base = filepath.Join(os.TempDir(), fmt.Sprintf("mountable-%d", os.Getuid()))
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", nil, err
	}
	// base may be reused, even made by someone else in a shared /tmp.
	if err := checkPrivate(base); err != nil {
		return "", nil, err
	}
	root, err := lockFile(filepath.Join(base, cacheLockName), unix.LOCK_EX)
	if err != nil {
		return "", nil, err
	}
	if root != nil {
		defer root.Close()
		pruneCacheDirs(base)
	}
	dir, err := os.MkdirTemp(base, "mount-")
	if err != nil {
		return "", nil, err
	}
	lock, err := lockCacheDir(dir)
	if err != nil {
		os.RemoveAll(dir)
		return "", nil, err
	}
	return dir, func() {
		if lock != nil {
			lock.Close()
		}
	}, nil
}

// cacheLockName is the lock file of the cache root, held while a mount
// creates or prunes directories, and of each mount's cache directory, held
// by its process while it runs and naming its host; a lock goes with its
// process.
const cacheLockName = "lock"

// flock is unix.Flock, replaceable in tests.
var flock = unix.Flock

// lockFile opens path, made if new, with the lock how asks for, or returns
// nil when the filesystem has no flock.
func lockFile(path string, how int) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := flock(int(f.Fd()), how); err != nil {
		f.Close()
		if errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOLCK) {
			return nil, nil
		}
		return nil, err
	}
	return f, nil
}

// lockCacheDir writes this host's name into dir's lock file and locks it
// for the mount's lifetime. Where the filesystem has no flock, the lock
// file goes, so no pruner ever takes the directory for a dead mount's.
func lockCacheDir(dir string) (*os.File, error) {
	path := filepath.Join(dir, cacheLockName)
	host, _ := os.Hostname()
	if err := os.WriteFile(path, []byte(host), 0o600); err != nil {
		return nil, err
	}
	lock, err := lockFile(path, unix.LOCK_EX|unix.LOCK_NB)
	if err == nil && lock == nil {
		err = os.Remove(path)
	}
	return lock, err
}

// pruneCacheDirs removes the cache directories in base that this host's
// mounts left: their lock file names this host and its lock is free. The
// caller holds the root's lock. A directory with no lock file (an older
// CLI, or one without flock) or naming another host (a home shared over
// the network, where locks may be local to each host) stays.
func pruneCacheDirs(base string) {
	host, err := os.Hostname()
	if err != nil {
		return
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "mount-") {
			continue
		}
		dir := filepath.Join(base, e.Name())
		path := filepath.Join(dir, cacheLockName)
		if owner, err := os.ReadFile(path); err != nil || string(owner) != host {
			continue
		}
		if lock, err := lockFile(path, unix.LOCK_EX|unix.LOCK_NB); err == nil && lock != nil {
			os.RemoveAll(dir)
			lock.Close()
		}
	}
}

// unmount is `mountable unmount DIR`: a clean unmount, after which the
// mounting process finishes its pending writes and exits. A mount that
// cannot be unmounted cleanly (busy, or not answering) is aborted instead.
func unmount(given string) error {
	dir, err := canonicalDir(given)
	if err != nil {
		return err
	}
	if cleared, err := clearStaleMount(dir); cleared || err != nil {
		return err
	}
	err = cleanUnmount(dir)
	if err == nil {
		return nil
	}
	if notMounted(err) {
		return &cliError{Code: "not_mounted", Message: given + " is not mounted"}
	}
	why := fmt.Sprintf("%s could not be unmounted cleanly (%s)", dir, oneLine(err))
	diagnose("%s; aborting the mount", why)
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
