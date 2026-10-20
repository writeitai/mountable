package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// abortConnection force-unmounts dir: every pending and further operation in
// it fails.
func abortConnection(dir string) error {
	err := withTimeout(helperTimeout, func() error { return unix.Unmount(dir, unix.MNT_FORCE) })
	if err == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), helperTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "diskutil", "unmount", "force", dir).Run()
}

// detach has nothing left to do on macOS: the forced unmount detached dir.
func detach(string) error { return nil }

// cleanUnmount unmounts dir; it fails while the mount is busy.
func cleanUnmount(dir string) error {
	return withTimeout(helperTimeout, func() error { return unix.Unmount(dir, 0) })
}

// mountTable reads the OS mount table. MNT_NOWAIT returns what the kernel
// has without asking any mount's server, so a dead mount cannot hang it.
func mountTable() ([]unix.Statfs_t, error) {
	n, err := unix.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return nil, err
	}
	table := make([]unix.Statfs_t, n+16) // room for mounts added meanwhile
	n, err = unix.Getfsstat(table, unix.MNT_NOWAIT)
	if err != nil {
		return nil, err
	}
	return table[:n], nil
}

// mountAt returns the topmost mount on dir, from the mount table alone.
func mountAt(dir string) (unix.Statfs_t, bool) {
	table, err := mountTable()
	if err != nil {
		return unix.Statfs_t{}, false
	}
	var found unix.Statfs_t
	ok := false
	for _, m := range table {
		if cString(m.Mntonname[:]) == dir {
			found, ok = m, true
		}
	}
	return found, ok
}

func cString(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}

// mountableSocket returns the socket behind one of this user's Mountable
// NFS mounts, whose source is "<…/nfs.sock>:/" in a socket directory of
// this CLI.
func mountableSocket(m *unix.Statfs_t) (string, bool) {
	if cString(m.Fstypename[:]) != "nfs" || m.Owner != uint32(os.Getuid()) {
		return "", false
	}
	socket, ok := strings.CutPrefix(cString(m.Mntfromname[:]), "<")
	if !ok {
		return "", false
	}
	socket, ok = strings.CutSuffix(socket, ">:/")
	if !ok || filepath.Base(socket) != socketName || !strings.Contains(socket, "/mountable") {
		return "", false
	}
	return socket, true
}

// served reports whether a process still listens on socket.
func served(socket string) bool {
	c, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

// isMountPoint reports whether the mount table lists path as a mount point,
// so canonicalDir leaves it alone: reading its link would reach its server.
func isMountPoint(path string) bool {
	_, ok := mountAt(path)
	return ok
}

// clearStaleMount force-unmounts a Mountable mount at dir whose process is
// gone, reporting whether there was one. An NFS mount outlives its server,
// and a new mount on top of a dead one would hang.
func clearStaleMount(dir string) (bool, error) {
	m, ok := mountAt(dir)
	if !ok {
		return false, nil
	}
	socket, ours := mountableSocket(&m)
	if !ours || served(socket) {
		return false, nil
	}
	diagnose("%s is left over from a mount whose process ended; forcing it off", dir)
	if err := abortConnection(dir); err != nil {
		return true, fmt.Errorf("forcing off the stale mount at %s: %w", dir, err)
	}
	return true, nil
}

// prepareMountPoint readies dir for mounting before anything touches it:
// a stale Mountable mount there is forced off, any other mount refused,
// and dir must be the user's own, as macOS requires of a mount by a user
// who is not root.
func prepareMountPoint(dir string) error {
	if _, err := clearStaleMount(dir); err != nil {
		return err
	}
	if _, ok := mountAt(dir); ok {
		return &cliError{Code: "mount_failed", Message: dir + " is already a mount point", Hint: "unmount it first: mountable unmount " + dir}
	}
	var st syscall.Stat_t
	if err := withTimeout(helperTimeout, func() error { return syscall.Stat(dir, &st) }); err != nil {
		return nil // the directory check that follows reports it
	}
	if st.Uid != uint32(os.Getuid()) {
		return &cliError{
			Code:    "mount_failed",
			Message: fmt.Sprintf("macOS mounts only on a directory you own; %s belongs to uid %d", dir, st.Uid),
			Hint:    "mount on a directory you created, such as: mkdir -p ~/mnt",
		}
	}
	return nil
}
