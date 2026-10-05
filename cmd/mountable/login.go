package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// credentials are kept in an owner-only file.
type credentials struct {
	API          string    `json:"api"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
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

func saveCredentials(t tokenResponse) error {
	path, err := credentialsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(credentials{
		API: apiURL(), AccessToken: t.AccessToken, RefreshToken: t.RefreshToken,
		ExpiresAt: time.Now().Add(time.Duration(t.ExpiresIn) * time.Second),
	})
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

// accessToken returns a valid access token, refreshing it when needed.
func accessToken() (string, error) {
	path, err := credentialsPath()
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", errors.New("not signed in; run `mountable login`")
	}
	if err != nil {
		return "", err
	}
	var c credentials
	if err := json.Unmarshal(data, &c); err != nil {
		return "", err
	}
	if c.API != apiURL() {
		return "", fmt.Errorf("signed in to %s, not %s; run `mountable login`", c.API, apiURL())
	}
	if time.Until(c.ExpiresAt) > time.Minute {
		return c.AccessToken, nil
	}
	var t tokenResponse
	err = postForm("/auth/device/token", url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {c.RefreshToken},
	}, &t)
	if err != nil {
		return "", fmt.Errorf("session ended (%v); run `mountable login`", err)
	}
	return t.AccessToken, saveCredentials(t)
}

func login() error {
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
	fmt.Printf("Open %s\nand check that it shows the code %s\n", code.VerificationURIComplete, code.UserCode)
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
		fmt.Println("Signed in.")
		return saveCredentials(t)
	}
	return errors.New("the code expired; run `mountable login` again")
}

func logout() error {
	token, err := accessToken()
	if err != nil {
		return err
	}
	if err := call("POST", "/auth/cli/sign-out", token, nil, nil); err != nil {
		return err
	}
	path, err := credentialsPath()
	if err != nil {
		return err
	}
	return os.Remove(path)
}
