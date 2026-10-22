package main

import "golang.org/x/sys/unix"

// localTypes are the filesystem types whose flock is the kernel's own;
// anything else (NFS, SMB, FUSE, a type not listed) is not taken for local.
var localTypes = map[int64]bool{
	0xEF53:     true, // ext2, ext3, ext4
	0x58465342: true, // xfs
	0x9123683E: true, // btrfs
	0x01021994: true, // tmpfs
	0x794C7630: true, // overlayfs
	0x2FC12FC1: true, // zfs
	0xF2F52010: true, // f2fs
	0xCA451A4E: true, // bcachefs
}

// localFilesystem reports whether path is on a local filesystem.
var localFilesystem = func(path string) bool {
	var st unix.Statfs_t
	return unix.Statfs(path, &st) == nil && localTypes[int64(st.Type)]
}
