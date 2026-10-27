package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// WinFsp is installed by the user, once per machine, and loaded from its
// own directory (design §2).

// minWinFsp is the oldest WinFsp build the mount accepts: 2.2 Beta 5
// (2.2.26286), the first with the fixes for CVE-2026-3006, CVE-2026-7162
// and the deadlocks in cached writes and notifications (design §2). The
// stable 2.1 has none of them.
var minWinFsp = [3]uint16{2, 2, 26286}

const winfspHint = "install WinFsp 2.2 Beta 5 or later, as an administrator, once: " +
	"https://github.com/winfsp/winfsp/releases/download/v2.2B5/winfsp-2.2.26286.msi — " +
	"WinFsp - Windows File System Proxy, Copyright (C) Bill Zissimopoulos, https://github.com/winfsp/winfsp"

var winfspOnce struct {
	sync.Once
	err error
}

// loadWinFsp finds WinFsp from its registration in HKLM (writable only by
// administrators), checks that its files are writable only by
// administrators and that the DLL and the driver are recent enough, and
// loads the DLL by absolute path. cgofuse later loads the DLL by its bare
// name, which Windows answers with the module already loaded; and bare
// names are searched for in System32 alone from then on. So the DLL is
// never taken from the current directory, PATH or the CLI's own directory.
func loadWinFsp() error {
	winfspOnce.Do(func() { winfspOnce.err = findAndLoadWinFsp() })
	return winfspOnce.err
}

func findAndLoadWinFsp() error {
	missing := func(detail string) error {
		return &cliError{Code: "winfsp_missing", Message: "mounting on Windows needs WinFsp, which is not installed (" + detail + ")", Hint: winfspHint}
	}
	dir, err := winfspInstallDir()
	if err != nil {
		return missing("no WinFsp registration: " + err.Error())
	}
	bin := filepath.Join(dir, "bin")
	dll, driver := filepath.Join(bin, winfspFile(".dll")), filepath.Join(bin, winfspFile(".sys"))
	for _, path := range []string{dll, driver} {
		if _, err := os.Stat(path); err != nil {
			return missing(err.Error())
		}
	}
	for _, path := range []string{dir, bin, dll, driver} {
		if err := adminOnly(path); err != nil {
			return &cliError{Code: "winfsp_unsafe", Message: err.Error(), Hint: "reinstall WinFsp in its default location, under Program Files"}
		}
	}
	for _, path := range []string{dll, driver} {
		version, err := fileVersion(path)
		if err != nil {
			return missing("reading the version of " + path + ": " + err.Error())
		}
		if !atLeast(version, minWinFsp) {
			return &cliError{
				Code: "winfsp_too_old",
				Message: fmt.Sprintf("%s is WinFsp %d.%d.%d; mounting needs %d.%d.%d or later",
					filepath.Base(path), version[0], version[1], version[2], minWinFsp[0], minWinFsp[1], minWinFsp[2]),
				Hint: winfspHint,
			}
		}
	}
	if err := windows.SetDefaultDllDirectories(windows.LOAD_LIBRARY_SEARCH_SYSTEM32); err != nil {
		return err
	}
	if _, err := windows.LoadDLL(dll); err != nil {
		return missing("loading " + dll + ": " + err.Error())
	}
	return nil
}

// winfspInstallDir reads WinFsp's InstallDir, which its installer writes
// to the 32-bit registry view, where cgofuse looks too.
func winfspInstallDir() (string, error) {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\WinFsp`, registry.QUERY_VALUE|registry.WOW64_32KEY)
	if err != nil {
		return "", err
	}
	defer k.Close()
	dir, _, err := k.GetStringValue("InstallDir")
	if err != nil {
		return "", err
	}
	if dir == "" || !filepath.IsAbs(dir) {
		return "", fmt.Errorf("InstallDir %q is not an absolute path", dir)
	}
	return dir, nil
}

// winfspFile is WinFsp's DLL (".dll", the name cgofuse loads) or driver
// (".sys") for this process's architecture.
func winfspFile(ext string) string {
	if runtime.GOARCH == "arm64" {
		return "winfsp-a64" + ext
	}
	return "winfsp-x64" + ext
}

func atLeast(version, min [3]uint16) bool {
	for i := range version {
		if version[i] != min[i] {
			return version[i] > min[i]
		}
	}
	return true
}

// fileVersion reads the file version (major, minor, build) from path's
// version resource.
func fileVersion(path string) ([3]uint16, error) {
	size, err := windows.GetFileVersionInfoSize(path, nil)
	if err != nil {
		return [3]uint16{}, err
	}
	info := make([]byte, size)
	if err := windows.GetFileVersionInfo(path, 0, size, unsafe.Pointer(&info[0])); err != nil {
		return [3]uint16{}, err
	}
	var fixed *windows.VS_FIXEDFILEINFO
	var length uint32
	if err := windows.VerQueryValue(unsafe.Pointer(&info[0]), `\`, unsafe.Pointer(&fixed), &length); err != nil {
		return [3]uint16{}, err
	}
	if fixed == nil || length < uint32(unsafe.Sizeof(*fixed)) {
		return [3]uint16{}, errors.New("no fixed version information")
	}
	return [3]uint16{uint16(fixed.FileVersionMS >> 16), uint16(fixed.FileVersionMS), uint16(fixed.FileVersionLS >> 16)}, nil
}
