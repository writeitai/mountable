//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

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

// driveLetter is for Windows mount points.
func driveLetter(string) (string, bool) { return "", false }
