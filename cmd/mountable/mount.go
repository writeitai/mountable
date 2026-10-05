package main

import (
	"bufio"
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
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// The filesystem owner's uid/gid as stored by the Mountable gateway; the mount
// shows them as the local user.
const ownerID = 1000

// How often the certificate is renewed; a refused renewal means the session
// was revoked, and the mount is aborted.
const renewEvery = 2 * time.Minute

type mountCredentials struct {
	SessionID            string `json:"session_id"`
	Certificate          string `json:"certificate"`
	CACertificate        string `json:"ca_certificate"`
	CertificateExpiresIn int    `json:"certificate_expires_in"`
	GatewayAddress       string `json:"gateway_address"`
	Root                 string `json:"root"`
	Mode                 string `json:"mode"`
}

const securityTOML = `[grpc]
ca = "%[1]s/ca.pem"
[grpc.client]
cert = "%[1]s/session.pem"
key = "%[1]s/session.key"
[https.client]
enabled = true
cert = "%[1]s/session.pem"
key = "%[1]s/session.key"
ca = "%[1]s/ca.pem"
`

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

func mount(ticket, dir string) error {
	// Before the exchange: a ticket is single-use.
	weedPath, err := weedBinary()
	if err != nil {
		return err
	}
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

	// Credentials and caches live in a private directory removed on exit.
	work, err := os.MkdirTemp("", "mountable-mount-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return err
	}
	files := map[string]string{
		"session.key":   string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})),
		"session.pem":   creds.Certificate,
		"ca.pem":        creds.CACertificate,
		"security.toml": fmt.Sprintf(securityTOML, work),
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(work, name), []byte(content), 0o600); err != nil {
			return err
		}
	}

	weed := exec.Command(weedPath, weedArgs(work, creds, dir)...)
	// Pick up renewed certificates quickly (SeaweedFS rereads them on this
	// interval; its default is 5 hours).
	weed.Env = append(os.Environ(), "WEED_TLS_CERT_REFRESH_INTERVAL=1m")
	weed.Stdout, weed.Stderr = os.Stdout, os.Stderr
	if err := weed.Start(); err != nil {
		return fmt.Errorf("starting weed: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- weed.Wait() }()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	ticker := time.NewTicker(renewEvery)
	defer ticker.Stop()
	for {
		select {
		case err := <-exited:
			return err
		case <-signals:
			return stop(weed, exited, dir)
		case <-ticker.C:
			revoked, err := renew(key, creds.SessionID, filepath.Join(work, "session.pem"))
			if revoked {
				// Fail closed: cached data must not stay readable.
				fmt.Fprintln(os.Stderr, "mountable: the mount session was revoked or expired; unmounting")
				_ = stop(weed, exited, dir)
				return err
			}
			if err != nil {
				fmt.Fprintln(os.Stderr, "mountable: renewal failed, retrying:", err)
			}
		}
	}
}

func weedArgs(work string, creds mountCredentials, dir string) []string {
	args := []string{
		"-config_dir=" + work, "mount",
		"-filer=" + creds.GatewayAddress,
		"-filer.path=" + creds.Root,
		"-volumeServerAccess=filerProxy",
		"-dlm",
		"-dir=" + dir,
		"-allowOthers=false",
		"-cacheDir=" + filepath.Join(work, "cache"),
		fmt.Sprintf("-map.uid=%d:%d", os.Getuid(), ownerID),
		fmt.Sprintf("-map.gid=%d:%d", os.Getgid(), ownerID),
	}
	if creds.Mode == "ro" {
		args = append(args, "-readOnly")
	}
	return args
}

// renew gets a new certificate for the same key. revoked is true when the
// API refused: the session no longer exists or lost its authority.
func renew(key *ecdsa.PrivateKey, sessionID, certPath string) (revoked bool, err error) {
	csr, err := certificateRequest(key)
	if err != nil {
		return false, err
	}
	var creds mountCredentials
	err = call("POST", "/api/v1/mount-sessions/"+sessionID+":renew", "", map[string]string{"csr": csr}, &creds)
	var api *apiError
	if errors.As(err, &api) && (api.Status == 403 || api.Status == 404) {
		return true, err
	}
	if err != nil {
		return false, err
	}
	tmp := certPath + ".new"
	if err := os.WriteFile(tmp, []byte(creds.Certificate), 0o600); err != nil {
		return false, err
	}
	return false, os.Rename(tmp, certPath)
}

func stop(weed *exec.Cmd, exited chan error, dir string) error {
	_ = unmount(dir)
	_ = weed.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		_ = weed.Process.Kill()
	}
	return nil
}

func unmount(dir string) error {
	if runtime.GOOS == "linux" {
		if err := exec.Command("fusermount", "-uz", dir).Run(); err == nil {
			return nil
		}
		return exec.Command("umount", "-l", dir).Run()
	}
	return exec.Command("umount", "-f", dir).Run()
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
