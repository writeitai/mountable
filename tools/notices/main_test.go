package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLeadingComment(t *testing.T) {
	source := "//go:build linux\n\n// Copyright 2015 Someone\n// Licensed under MIT.\n\npackage x\n\n// Copyright later, not a header\n"
	if got, want := leadingComment(source), "// Copyright 2015 Someone\n// Licensed under MIT."; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := leadingComment("package x\n// Copyright 2015 Someone\n"); got != "" {
		t.Fatalf("a comment after the package clause was taken: %q", got)
	}
}

func TestForeignCopyright(t *testing.T) {
	known := normalize("Copyright (c) 2009 The Go Authors. All rights reserved.") + "\n" +
		normalize("copyright notice, this list of conditions and the following disclaimer.")
	cases := map[string]bool{
		"// Copyright 2021 The Go Authors. All rights reserved.":                        false,
		"// Copyright (c) 2012 Jeff Hodges. All rights reserved.":                       true,
		"// Copyright 2008 Google Inc.  All rights reserved.":                           true,
		"//    notice, this list of conditions and the following disclaimer. copyright": false,
		"// no claim here": false,
	}
	for header, want := range cases {
		if got := foreignCopyright(header, known); got != want {
			t.Errorf("foreignCopyright(%q) = %v, want %v", header, got, want)
		}
	}
}

// A file under its own license next to a module-wide one is reported once
// per distinct header, with every file that carries it.
func TestFileHeaders(t *testing.T) {
	root := t.TempDir()
	write := func(name, text string) string {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	license := write("LICENSE", "Copyright (c) 2015 Tyler Treat\n\nPermission is hereby granted...")
	sources := map[string]bool{
		write("own.go", "// Copyright 2015 Tyler Treat\npackage boom\n"):                                        true,
		write("inverse.go", "/*\nOriginal work Copyright (c) 2012 Jeff Hodges.\nBSD terms\n*/\npackage boom\n"): true,
		write("other.go", "/*\nOriginal work Copyright (c) 2012 Jeff Hodges.\nBSD terms\n*/\npackage boom\n"):   true,
	}
	headers, err := fileHeaders(root, []string{license}, sources)
	if err != nil {
		t.Fatal(err)
	}
	if len(headers) != 1 || strings.Join(headers[0].files, ",") != "inverse.go,other.go" || !strings.Contains(headers[0].text, "Jeff Hodges") {
		t.Fatalf("got %+v", headers)
	}
}
