package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// The lock is on the file's first byte, which LockFileEx allows past the
// end of an empty file. Like flock, it belongs to the open (the handle),
// and closing the handle releases it.

// lockFile takes an exclusive lock on f, waiting for it.
func lockFile(f *os.File) error {
	return lockFirstByte(f, windows.LOCKFILE_EXCLUSIVE_LOCK)
}

// tryLockFile takes an exclusive lock on f without waiting; it fails if
// another open of the file holds one.
func tryLockFile(f *os.File) error {
	return lockFirstByte(f, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY)
}

// unlockFile releases f's lock.
func unlockFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped))
}

func lockFirstByte(f *os.File, flags uint32) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, new(windows.Overlapped))
}
