//go:build !windows

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// lockFile takes an exclusive lock on f, waiting for it. Closing f
// releases it.
func lockFile(f *os.File) error {
	for {
		err := unix.Flock(int(f.Fd()), unix.LOCK_EX)
		if err != unix.EINTR {
			return err
		}
	}
}

// tryLockFile takes an exclusive lock on f without waiting; it fails if
// another open of the file holds one, or the filesystem has no flock.
func tryLockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

// unlockFile releases f's lock.
func unlockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
