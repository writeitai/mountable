package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// credentials are kept in an owner-only file.
type credentials struct {
	API          string    `json:"api"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	// Generation identifies one `mountable login`; refreshes keep it. A
	// request is retried only under the login it started with.
	Generation string `json:"generation"`
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

func credentialsPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "mountable", "credentials.json"), nil
}

// credentialsMu serializes this process's credential use (simultaneous MCP
// tool calls); the lock file serializes it across processes.
var credentialsMu sync.Mutex

// withCredentials runs fn while holding the credentials lock, so reading the
// stored login, deciding to refresh, redeeming the single-use refresh token
// and saving the new pair happen as one step.
func withCredentials(fn func() error) error {
	credentialsMu.Lock()
	defer credentialsMu.Unlock()
	path, err := credentialsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close() // closing releases the lock
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX)
		if err != unix.EINTR {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("locking %s: %w", lock.Name(), err)
	}
	return fn()
}

func saveCredentials(t tokenResponse, generation string) error {
	return withCredentials(func() error { return writeCredentials(t, generation) })
}

// writeCredentials replaces the credentials file atomically with an
// owner-only one. The caller holds the credentials lock.
func writeCredentials(t tokenResponse, generation string) error {
	path, err := credentialsPath()
	if err != nil {
		return err
	}
	data, err := json.Marshal(credentials{
		API: apiURL(), AccessToken: t.AccessToken, RefreshToken: t.RefreshToken,
		ExpiresAt:  time.Now().Add(time.Duration(t.ExpiresIn) * time.Second),
		Generation: generation,
	})
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".credentials-*.tmp") // mode 0600
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // fails harmlessly once renamed
	_, err = tmp.Write(data)
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync() // makes the rename durable where the platform supports it
		d.Close()
	}
	return nil
}

// accessToken returns a valid access token, refreshing it when needed, and
// the generation of the login it belongs to.
func accessToken() (token, generation string, err error) {
	err = withCredentials(func() error {
		c, err := loadCredentials()
		if err != nil {
			return err
		}
		generation = c.Generation
		if time.Until(c.ExpiresAt) > time.Minute {
			token = c.AccessToken
			return nil
		}
		token, err = refresh(c)
		return err
	})
	return token, generation, err
}

// refreshRejected returns a new access token for the same login after the
// API refused rejected: the stored one, when another caller has already
// refreshed this login, otherwise a refreshed one. It fails when the stored
// login is no longer generation (a new `mountable login` or a logout).
func refreshRejected(rejected, generation string) (token string, err error) {
	err = withCredentials(func() error {
		c, err := loadCredentials()
		if err != nil {
			return err
		}
		if c.Generation != generation {
			return errors.New("signed in again since the request was made")
		}
		if c.AccessToken != rejected {
			token = c.AccessToken
			return nil
		}
		token, err = refresh(c)
		return err
	})
	return token, err
}

func loadCredentials() (credentials, error) {
	var c credentials
	path, err := credentialsPath()
	if err != nil {
		return c, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, errors.New("not signed in; run `mountable login`")
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, err
	}
	if c.API != apiURL() {
		return c, fmt.Errorf("signed in to %s, not %s; run `mountable login`", c.API, apiURL())
	}
	return c, nil
}

// refresh exchanges the refresh token for a new pair and saves it; the
// refresh token rotates. The caller holds the credentials lock.
func refresh(c credentials) (string, error) {
	var t tokenResponse
	err := postForm("/auth/device/token", url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {c.RefreshToken},
	}, &t)
	if err != nil {
		return "", fmt.Errorf("session ended (%v); run `mountable login`", err)
	}
	return t.AccessToken, writeCredentials(t, c.Generation)
}

// login signs this machine in, writing its instructions to w.
func login(w io.Writer) error {
	host, _ := os.Hostname()
	var code struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
	}
	if err := postForm("/auth/device/code", url.Values{
		"hostname": {host}, "os": {runtime.GOOS},
	}, &code); err != nil {
		return err
	}
	fmt.Fprintf(w, "Open %s\nand check that it shows the code %s\n", code.VerificationURIComplete, code.UserCode)
	deadline := time.Now().Add(time.Duration(code.ExpiresIn) * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(time.Duration(code.Interval) * time.Second)
		var t tokenResponse
		err := postForm("/auth/device/token", url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"device_code": {code.DeviceCode},
		}, &t)
		var api *apiError
		if errors.As(err, &api) && api.Code == "authorization_pending" {
			continue
		}
		// A network error (e.g. the API closing an idle connection just as a
		// poll reuses it) is not an answer: poll again until the code expires.
		var network *url.Error
		if errors.As(err, &network) {
			continue
		}
		if err != nil {
			return err
		}
		if err := saveCredentials(t, randomHex(16)); err != nil {
			return err
		}
		fmt.Fprintln(w, "Signed in.")
		return nil
	}
	return errors.New("the code expired; run `mountable login` again")
}

// logout signs the login out and removes it, unless a new login replaced
// it while the sign-out was in flight.
func logout() error {
	c, err := loginClient("")
	if err != nil {
		return err
	}
	if err := c.call(context.Background(), "POST", "/auth/cli/sign-out", nil, nil); err != nil {
		return err
	}
	return withCredentials(func() error {
		stored, err := loadCredentials()
		if err != nil || stored.Generation != c.generation {
			return nil // already removed, or a newer login: keep it
		}
		path, err := credentialsPath()
		if err != nil {
			return err
		}
		return os.Remove(path)
	})
}
