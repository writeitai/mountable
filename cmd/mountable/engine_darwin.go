package main

// On macOS the engine is served to the Mac's own NFSv3 client from this
// process, through a Unix socket only this user (and root) can reach, and
// mounted with /sbin/mount_nfs as the user: nothing to install, no sudo.
// The design is design/designs/macos-mount.md in writeitai/fsit.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	weedmount "github.com/seaweedfs/seaweedfs/weed/mount"
	"github.com/writeitai/mountable/internal/nfs"
	"golang.org/x/sys/unix"
)

// abortFallback is what still ends a mount the abort could not fully end:
// the mount's deadtimeout.
const abortFallback = "macOS drops it about 30 seconds after this process exits"

// socketName is the socket's name in its private directory.
const socketName = "nfs.sock"

// maxSocketPath is the longest path a Unix socket address holds.
const maxSocketPath = 103

// engine is one mount served by this process over NFS.
type engine struct {
	dir       string
	readOnly  bool
	wfs       *weedmount.WFS
	server    *nfs.Server
	socketDir string
	socket    string
	fsid      unix.Fsid // the mount's, once mounted
	done      chan struct{}
	endOnce   sync.Once
	stop      chan struct{} // ends the mount-table watch
	closeOnce sync.Once
}

// attach serves wfs on a private socket and mounts it at c.dir. The write
// path is not implemented yet, so every mount is read-only.
func attach(ctx context.Context, wfs *weedmount.WFS, c mountConfig) (*engine, error) {
	if !c.readOnly {
		diagnose("read-write mounts are not available on macOS yet; mounting read-only")
	}
	socketDir, err := makeSocketDir(c.cacheDir)
	if err != nil {
		return nil, fmt.Errorf("creating the NFS socket's directory: %w", err)
	}
	server, err := nfs.NewServer(nfs.Config{Engine: wfs, UID: uint32(os.Getuid())})
	if err != nil {
		os.RemoveAll(socketDir)
		return nil, err
	}
	e := &engine{
		dir:       c.dir,
		readOnly:  true,
		wfs:       wfs,
		server:    server,
		socketDir: socketDir,
		socket:    filepath.Join(socketDir, socketName),
		done:      make(chan struct{}),
		stop:      make(chan struct{}),
	}
	listener, err := listenSocket(e.socket)
	if err != nil {
		e.close()
		return nil, fmt.Errorf("listening on %s: %w", e.socket, err)
	}
	go server.Serve(listener)
	if err := e.mount(ctx); err != nil {
		// Never leave a partial mount behind.
		if m, ok := mountAt(e.dir); ok && cString(m.Mntfromname[:]) == e.source() {
			if abortErr := abortConnection(e.dir); abortErr != nil {
				err = fmt.Errorf("%w; cleaning up the mount: %v", err, abortErr)
			}
		}
		e.close()
		return nil, fmt.Errorf("mounting %s: %w", c.dir, err)
	}
	go e.watch()
	return e, nil
}

// source is the mount's source as mount_nfs takes it and the mount table
// lists it: the socket path in literal angle brackets.
func (e *engine) source() string { return "<" + e.socket + ">:/" }

// mount runs mount_nfs, then checks that the mount is what was asked for
// and that its root can be read.
func (e *engine) mount(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, "/sbin/mount_nfs", "-o", nfsOptions(e.socket, e.readOnly), e.source(), e.dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("mount_nfs: %v: %s", err, bytes.TrimSpace(out))
	}
	m, ok := mountAt(e.dir)
	if !ok || cString(m.Mntfromname[:]) != e.source() {
		return errors.New("mount_nfs succeeded, but the mount is not in the mount table")
	}
	e.fsid = m.Fsid
	if err := checkMountFlags(&m, e.readOnly); err != nil {
		return err
	}
	checkCtx, cancel := context.WithTimeout(ctx, helperTimeout)
	defer cancel()
	out, err := exec.CommandContext(checkCtx, "/usr/bin/nfsstat", "-m", e.dir).Output()
	if err != nil {
		return fmt.Errorf("nfsstat -m: %w", err)
	}
	if err := checkMountParameters(string(out)); err != nil {
		return err
	}
	return unlessCancelled(checkCtx, func() error { return readRoot(e.dir) })
}

// nfsOptions are the mount options (design §2). Every one that matters for
// safety is explicit, because mount_nfs applies /etc/nfs.conf first.
func nfsOptions(socket string, readOnly bool) string {
	options := []string{
		"vers=3", "proto=ticotsord", "port=" + socket, "mountport=" + socket,
		"opaque_auth", "noasync", "nonfc", "noresvport",
		"locallocks", "noquota", "noacl",
		"actimeo=1", "nonegnamecache",
		"rsize=1048576", "wsize=1048576", "dsize=65536",
		"hard", "nodev", "nosuid", "intr", "mutejukebox", "dumbtimer", "timeo=300", "deadtimeout=30",
	}
	if readOnly {
		options = append(options, "ro")
	}
	return strings.Join(options, ",")
}

// checkMountFlags checks the mount table's flags for the mount.
func checkMountFlags(m *unix.Statfs_t, readOnly bool) error {
	switch {
	case cString(m.Fstypename[:]) != "nfs":
		return fmt.Errorf("mounted as %s, not nfs", cString(m.Fstypename[:]))
	case m.Flags&unix.MNT_ASYNC != 0:
		return errors.New("mounted async")
	case (m.Flags&unix.MNT_RDONLY != 0) != readOnly:
		return fmt.Errorf("mounted with read-only %v, want %v", m.Flags&unix.MNT_RDONLY != 0, readOnly)
	case m.Flags&unix.MNT_NOSUID == 0:
		return errors.New("mounted without nosuid")
	case m.Flags&unix.MNT_NODEV == 0:
		return errors.New("mounted without nodev")
	}
	return nil
}

// checkMountParameters checks what `nfsstat -m` reports the kernel uses:
// NFSv3 over the local stream socket, hard, and the transfer sizes asked
// for.
func checkMountParameters(nfsstat string) error {
	_, current, ok := strings.Cut(nfsstat, "Current mount parameters:")
	if !ok {
		return errors.New("nfsstat -m reported no current mount parameters")
	}
	var params string
	for _, line := range strings.Split(current, "\n") {
		if p, ok := strings.CutPrefix(strings.TrimSpace(line), "NFS parameters:"); ok {
			params = p
			break
		}
	}
	have := map[string]bool{}
	for _, p := range strings.Split(params, ",") {
		have[strings.TrimSpace(p)] = true
	}
	for _, want := range []string{"vers=3", "ticotsord", "hard", "rsize=1048576", "wsize=1048576", "dsize=65536"} {
		if !have[want] {
			return fmt.Errorf("the mount does not have %s (nfsstat -m: %s)", want, strings.TrimSpace(params))
		}
	}
	return nil
}

// readRoot reads the first name in the mounted root.
func readRoot(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Readdirnames(1); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("reading the mounted root: %w", err)
	}
	return nil
}

// makeSocketDir creates a fresh directory only this user can enter, for
// the socket: in the mount's private cache directory, or under
// /tmp/mountable-<uid>/ when a socket path there would be too long.
func makeSocketDir(cacheDir string) (string, error) {
	if dir := filepath.Join(cacheDir, "nfs"); socketPathFits(filepath.Join(dir, socketName)) {
		return dir, os.Mkdir(dir, 0o700)
	}
	base := fmt.Sprintf("/tmp/mountable-%d", os.Getuid())
	if err := os.Mkdir(base, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	if err := checkPrivate(base); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(base, "nfs-")
	if err == nil && !socketPathFits(filepath.Join(dir, socketName)) {
		os.Remove(dir)
		return "", fmt.Errorf("%s: socket path too long", dir)
	}
	return dir, err
}

// listenSocket listens on a Unix socket at path that only this user can
// connect to; its directory is private as well.
func listenSocket(path string) (net.Listener, error) {
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// socketPathFits reports whether path can be a socket address and a mount
// option value: short enough, and without the option list's separator or
// the source's brackets.
func socketPathFits(path string) bool {
	return len(path) <= maxSocketPath && !strings.ContainsAny(path, ",<>")
}

// checkPrivate checks that a reused directory is this user's alone.
func checkPrivate(dir string) error {
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || st.Uid != uint32(os.Getuid()) || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("%s must be a directory of yours with mode 0700; remove it and mount again", dir)
	}
	return nil
}

// watch notices the mount leaving the mount table, however it was
// unmounted. UMNT from the client is advisory and not trusted for this.
func (e *engine) watch() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-tick.C:
			if !e.mounted() {
				e.ended()
				return
			}
		}
	}
}

// mounted reports whether this mount, by its source and fsid, is still in
// the mount table. A table that cannot be read counts as mounted.
func (e *engine) mounted() bool {
	table, err := mountTable()
	if err != nil {
		return true
	}
	for _, m := range table {
		if cString(m.Mntonname[:]) == e.dir && cString(m.Mntfromname[:]) == e.source() && m.Fsid == e.fsid {
			return true
		}
	}
	return false
}

func (e *engine) ended() { e.endOnce.Do(func() { close(e.done) }) }

// close stops serving: the connections, the socket and its directory.
func (e *engine) close() {
	e.closeOnce.Do(func() {
		close(e.stop)
		e.server.Close()
		os.RemoveAll(e.socketDir)
	})
}

func (e *engine) mountPoint() string { return e.dir }

func (e *engine) served() <-chan struct{} { return e.done }

// unmount unmounts cleanly while still serving, so the kernel can send its
// last calls, and waits until the mount is gone. It fails while the mount
// is busy.
func (e *engine) unmount() error {
	if err := cleanUnmount(e.dir); err != nil {
		return err
	}
	e.ended()
	return nil
}

// flush runs once the mount is gone: it waits for pending writes, stops
// serving and drops the local caches.
func (e *engine) flush() {
	e.wfs.WaitForAsyncFlush()
	e.close()
	e.wfs.ClearCacheDir()
}

// abort ends the mount at once without flushing: every call fails from
// now on, the mount is forced off, and serving stops. It fails unless the
// mount is then gone from the mount table.
func (e *engine) abort() error {
	e.server.End()
	err := abortMount(e.dir)
	e.close()
	if err == nil && e.mounted() {
		err = errors.New("the mount is still in the mount table")
	}
	if err == nil {
		e.ended()
	}
	return err
}
