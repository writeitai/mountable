package main

import (
	"context"
	"os/exec"

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
