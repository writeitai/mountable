package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/writeitai/mountable"
)

func TestVersionPrintsOnlyTheCLIVersion(t *testing.T) {
	saved := version
	defer func() { version = saved }()
	version = "1.2.3"
	var out bytes.Buffer
	printVersion(&out)
	if out.String() != "mountable 1.2.3\n" {
		t.Fatalf("version printed %q", out.String())
	}
}

func TestLicensesPrintsTheEmbeddedNotices(t *testing.T) {
	var out bytes.Buffer
	printLicenses(&out)
	if out.String() != mountable.ThirdPartyNotices {
		t.Fatal("licenses did not print the embedded notices")
	}
	for _, want := range []string{"The Go standard library and runtime", "Apache License"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("notices lack %q", want)
		}
	}
}
