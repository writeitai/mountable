//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"strconv"

	"github.com/seaweedfs/seaweedfs/weed/util"
)

// checkMountDir checks the mount point before the ticket is exchanged: an
// existing directory.
func checkMountDir(dir string) error { return checkMountDirectory(dir) }

// mountOwner reads the owner from the mount point; one owned by root shows
// as the user's own, as upstream `weed mount` does.
func mountOwner(dir string) (owner, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return owner{}, err
	}
	uid, gid := util.GetFileUidGid(info)
	if uid == 0 {
		if u, err := user.Current(); err == nil {
			if id, err := strconv.ParseUint(u.Uid, 10, 32); err == nil {
				uid = uint32(id)
			}
			if id, err := strconv.ParseUint(u.Gid, 10, 32); err == nil {
				gid = uint32(id)
			}
		}
	}
	return owner{uid: uid, gid: gid, localUID: os.Getuid(), localGID: os.Getgid(), ctime: info.ModTime()}, nil
}

// checkBeforeSession has nothing to check before a session is created; the
// mount point is checked before its ticket is exchanged.
func checkBeforeSession(string) error { return nil }

// mountedPath is the path the mounted event reports.
func mountedPath(dir string) string { return dir }

// serveControl has nothing to serve: `mountable unmount` reaches the mount
// through the OS.
func serveControl(string, chan<- os.Signal) (func(error), error) {
	return func(error) {}, nil
}

// abortMount aborts the connection behind dir, then detaches dir. An error
// means the connection was not aborted (a lazy detach alone leaves open
// files working) or dir was not detached.
func abortMount(dir string) error {
	var errs []error
	if err := abortConnection(dir); err != nil {
		errs = append(errs, err)
	}
	if err := detach(dir); err != nil {
		errs = append(errs, fmt.Errorf("detaching: %w", err))
	}
	return errors.Join(errs...)
}
