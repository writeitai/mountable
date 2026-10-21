//go:build !darwin

package main

import (
	"context"
	"fmt"

	"github.com/seaweedfs/go-fuse/v2/fuse"
	weedmount "github.com/seaweedfs/seaweedfs/weed/mount"
)

// abortFallback is what still ends a mount the abort could not fully end.
const abortFallback = "it ends when this process exits"

// engine is one mount served by this process over FUSE.
type engine struct {
	dir      string
	readOnly bool
	wfs      *weedmount.WFS
	server   *fuse.Server
	done     chan struct{} // closed when the FUSE server stops serving
}

// attach mounts wfs at c.dir over FUSE.
func attach(ctx context.Context, wfs *weedmount.WFS, c mountConfig) (*engine, error) {
	server, err := fuse.NewServer(wfs, c.dir, fuseOptions(c.readOnly))
	if err != nil {
		return nil, fmt.Errorf("mounting %s: %w", c.dir, err)
	}
	e := &engine{dir: c.dir, readOnly: c.readOnly, wfs: wfs, server: server, done: make(chan struct{})}
	go func() {
		server.Serve()
		close(e.done)
	}()
	if err := unlessCancelled(ctx, server.WaitMount); err != nil {
		// Attached but not serving: never leave it behind.
		if abortErr := e.abort(); abortErr != nil {
			err = fmt.Errorf("%w; cleaning up the mount: %v", err, abortErr)
		}
		return nil, fmt.Errorf("mounting %s: %w", c.dir, err)
	}
	return e, nil
}

func fuseOptions(readOnly bool) *fuse.MountOptions {
	options := &fuse.MountOptions{
		AllowOther:    false,
		MaxBackground: 128,
		MaxWrite:      2 * 1024 * 1024,
		MaxReadAhead:  2 * 1024 * 1024,
		FsName:        "mountable",
		Name:          "mountable",
		EnableLocks:   true,
		DirectMount:   true,
		EnableAcl:     true,
		Options:       []string{"default_permissions", "nosuid", "nodev"},
	}
	if readOnly {
		options.Options = append(options.Options, "ro")
	}
	return options
}

func (e *engine) mountPoint() string { return e.dir }

func (e *engine) served() <-chan struct{} { return e.done }

// unmount ends the mount cleanly and waits for serving to stop. It fails
// while the mount is busy.
func (e *engine) unmount() error {
	if err := e.server.Unmount(); err != nil {
		return err
	}
	<-e.done
	return nil
}

// flush runs once serving has stopped: it waits for pending writes, then
// drops the local caches.
func (e *engine) flush() {
	e.wfs.WaitForAsyncFlush()
	e.wfs.ClearCacheDir()
}

// close has nothing to release: serving ends with the FUSE server.
func (e *engine) close() {}

// abort ends the mount at once without flushing: every further operation in
// it fails. Exiting the process afterwards also closes the FUSE device,
// which ends the connection even where the abort itself could not be done.
func (e *engine) abort() error { return abortMount(e.dir) }
