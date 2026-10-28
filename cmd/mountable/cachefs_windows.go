package main

import "golang.org/x/sys/windows"

// localFilesystem reports whether path is on a local disk, whose locks are
// this machine's own; a network share is not taken for local.
var localFilesystem = func(path string) bool {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	volume := make([]uint16, windows.MAX_PATH+1)
	if windows.GetVolumePathName(p, &volume[0], uint32(len(volume))) != nil {
		return false
	}
	switch windows.GetDriveType(&volume[0]) {
	case windows.DRIVE_FIXED, windows.DRIVE_RAMDISK:
		return true
	}
	return false
}
