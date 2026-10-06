package main

import (
	"path/filepath"
	"strconv"
	"strings"
)

// fuseConnection finds the FUSE mount at dir in /proc/self/mountinfo and
// returns its connection id: the name of its directory under
// /sys/fs/fuse/connections, which is the mount's device number.
func fuseConnection(mountinfo []byte, dir string) (string, bool) {
	dir = filepath.Clean(dir)
	id, found := "", false
	for _, line := range strings.Split(string(mountinfo), "\n") {
		// 36 35 0:52 / /mnt/f rw,nosuid shared:1 - fuse.mountable mountable rw
		fields := strings.Fields(line)
		separator := -1
		for i, f := range fields {
			if f == "-" {
				separator = i
				break
			}
		}
		if separator < 6 || separator+1 >= len(fields) {
			continue
		}
		fsType := fields[separator+1]
		if fsType != "fuse" && !strings.HasPrefix(fsType, "fuse.") {
			continue
		}
		if unescapeMountinfo(fields[4]) != dir {
			continue
		}
		majorText, minorText, ok := strings.Cut(fields[2], ":")
		major, err1 := strconv.ParseUint(majorText, 10, 32)
		minor, err2 := strconv.ParseUint(minorText, 10, 32)
		if !ok || err1 != nil || err2 != nil {
			continue
		}
		// The kernel's dev_t: MKDEV(major, minor). The last match is the
		// mount on top.
		id, found = strconv.FormatUint(major<<20|minor, 10), true
	}
	return id, found
}

// unescapeMountinfo decodes the octal escapes (\040 for a space) the kernel
// writes for whitespace and backslashes in paths.
func unescapeMountinfo(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
