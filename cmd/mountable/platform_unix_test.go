//go:build !windows

package main

import (
	"path/filepath"
	"testing"
)

// testMountPoint is a mount point the CLI accepts: an existing directory.
func testMountPoint(t *testing.T) string { return t.TempDir() }

// missingMountPoint is one it refuses for want of a directory.
func missingMountPoint(t *testing.T) string { return filepath.Join(t.TempDir(), "missing") }

// engineCacheDir is a cache directory for an engine a test leaves running.
func engineCacheDir(t *testing.T) string { return t.TempDir() }
