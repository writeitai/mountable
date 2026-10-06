package main

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

const fuseConnections = "/sys/fs/fuse/connections"

// abortConnection aborts the FUSE connection behind dir: the kernel fails
// every pending and further operation in it.
func abortConnection(dir string) error {
	mountinfo, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return err
	}
	id, ok := fuseConnection(mountinfo, dir)
	if !ok {
		return fmt.Errorf("%s is not a FUSE mount", dir)
	}
	abort := filepath.Join(fuseConnections, id, "abort")
	if _, err := os.Stat(abort); errors.Is(err, fs.ErrNotExist) {
		// The FUSE control filesystem is not mounted; mounting it needs root.
		_ = syscall.Mount("fusectl", fuseConnections, "fusectl", 0, "")
	}
	file, err := os.OpenFile(abort, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = file.Write([]byte("1"))
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	return err
}

// detach lazily unmounts dir: it leaves the namespace now, whatever still
// uses it.
func detach(dir string) error {
	if err := syscall.Unmount(dir, syscall.MNT_DETACH); err == nil {
		return nil
	}
	return fusermount(dir, "-uz")
}

// cleanUnmount unmounts dir; it fails while the mount is busy.
func cleanUnmount(dir string) error {
	err := syscall.Unmount(dir, 0)
	if !errors.Is(err, syscall.EPERM) {
		return err
	}
	// Not root: the setuid helper unmounts the user's own FUSE mounts.
	return fusermount(dir, "-u")
}

func fusermount(dir, flags string) error {
	var err error
	for _, helper := range []string{"fusermount3", "fusermount"} {
		var output []byte
		output, err = exec.Command(helper, flags, dir).CombinedOutput()
		if err == nil {
			return nil
		}
		if !errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("%s: %s", helper, bytes.TrimSpace(output))
		}
	}
	return err
}
