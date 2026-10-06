package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// helperTimeout bounds each unmount syscall and helper process.
const helperTimeout = 10 * time.Second

// canonicalDir returns dir as the kernel lists the mount: absolute, with
// symlinks resolved. Only the parent is resolved by walking, so a mount
// point whose server hangs is never itself stat-ed; a symlinked last
// component is followed by reading the link.
func canonicalDir(dir string) (string, error) {
	for range 40 {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return "", err
		}
		parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
		if err != nil {
			return "", err
		}
		path := filepath.Join(parent, filepath.Base(abs))
		target, err := os.Readlink(path)
		if err != nil {
			return path, nil
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(parent, target)
		}
		dir = target
	}
	return "", fmt.Errorf("%s: too many levels of symbolic links", dir)
}

// withTimeout runs fn, giving up after d; fn keeps running in the
// background if it hangs.
func withTimeout(d time.Duration, fn func() error) error {
	result := make(chan error, 1)
	go func() { result <- fn() }()
	select {
	case err := <-result:
		return err
	case <-time.After(d):
		return errors.New("timed out")
	}
}
