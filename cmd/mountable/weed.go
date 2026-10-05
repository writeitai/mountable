package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
)

// The SeaweedFS release the CLI runs (the version Mountable runs server-side). Raising it
// means new hashes here and a CLI release.
const weedVersion = "4.48"

// SHA-256 of each release archive at
// https://github.com/seaweedfs/seaweedfs/releases/download/4.48/<os>_<arch>.tar.gz,
// computed with `shasum -a 256` on 2026-10-05 (upstream publishes only MD5;
// those matched too).
var weedSHA256 = map[string]string{
	"linux_amd64":  "4a7d108384d044d95212d1342cdda9533fa55842c1c9b41f606ca3c8a9561124",
	"linux_arm64":  "557e92c38c5c7d180748eb8e2b2b45e426f8e5282a4caca8e098a90d09f03a07",
	"darwin_amd64": "425ef61d2ee773d5d5175003f38bd6cc891070ae6319370354aaf5378c45d5b3",
	"darwin_arm64": "faa194e72a346755dc4404288b0561eb3c260351702901d8363024d5099bdd7d",
}

// weedBinary returns MOUNTABLE_WEED, or the pinned weed in the user cache
// directory, downloading it on first use.
func weedBinary() (string, error) {
	if v := os.Getenv("MOUNTABLE_WEED"); v != "" {
		return v, nil
	}
	platform := runtime.GOOS + "_" + runtime.GOARCH
	sum, ok := weedSHA256[platform]
	if !ok {
		return "", fmt.Errorf("no SeaweedFS %s build for %s; set MOUNTABLE_WEED", weedVersion, platform)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	url := "https://github.com/seaweedfs/seaweedfs/releases/download/" + weedVersion + "/" + platform + ".tar.gz"
	return ensureWeed(filepath.Join(cache, "mountable", "weed-"+weedVersion, "weed"), url, sum)
}

// ensureWeed returns path, first downloading the archive at url, checking
// its SHA-256 against sum and extracting weed to path if path is missing.
func ensureWeed(path, url, sum string) (string, error) {
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	// A temporary file renamed into place: concurrent mounts never see a
	// partial binary, and a failed download leaves nothing behind.
	tmp, err := os.CreateTemp(filepath.Dir(path), "weed-*.tmp")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	err = download(tmp, url, sum)
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", fmt.Errorf("fetching SeaweedFS %s: %w", weedVersion, err)
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	fmt.Fprintf(os.Stderr, "mountable: fetched SeaweedFS %s to %s\n", weedVersion, path)
	return path, nil
}

// download writes the weed binary from the tar.gz at url to out and fails
// unless the whole archive's SHA-256 is sum.
func download(out io.Writer, url, sum string) error {
	response, err := http.Get(url)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, response.Status)
	}
	hash := sha256.New()
	archive := io.TeeReader(response.Body, hash)
	extractErr := extractWeed(out, archive)
	// Hash the rest of the archive, then judge it before anything else.
	if _, err := io.Copy(io.Discard, archive); err != nil {
		return err
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != sum {
		return fmt.Errorf("checksum mismatch for %s: got %s, want %s", url, got, sum)
	}
	return extractErr
}

func extractWeed(out io.Writer, archive io.Reader) error {
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return err
	}
	files := tar.NewReader(gz)
	for {
		header, err := files.Next()
		if errors.Is(err, io.EOF) {
			return errors.New("no weed binary in the archive")
		}
		if err != nil {
			return err
		}
		if header.Typeflag == tar.TypeReg && header.Name == "weed" {
			_, err := io.Copy(out, files)
			return err
		}
	}
}
