package main

import "golang.org/x/sys/unix"

// localFilesystem reports whether path is on a filesystem of this Mac,
// where flock is the kernel's own (MNT_LOCAL); anything else, or an error,
// is not.
var localFilesystem = func(path string) bool {
	var st unix.Statfs_t
	return unix.Statfs(path, &st) == nil && st.Flags&unix.MNT_LOCAL != 0
}
