package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const defaultAPI = "https://mountable.io"

func apiURL() string {
	if v := os.Getenv("MOUNTABLE_API_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultAPI
}

var httpClient = &http.Client{Timeout: 60 * time.Second}

// apiError is a non-2xx answer; Code is the API's stable error code.
type apiError struct {
	Status int
	Code   string
}

func (e *apiError) Error() string { return fmt.Sprintf("api: %d %s", e.Status, e.Code) }

func call(method, path, token string, body any, out any) error {
	return callContext(context.Background(), method, path, token, body, out)
}

func callContext(ctx context.Context, method, path, token string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, apiURL()+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return do(req, out)
}

func postForm(path string, form url.Values, out any) error {
	req, err := http.NewRequest(http.MethodPost, apiURL()+path, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return do(req, out)
}

func do(req *http.Request, out any) error {
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var problem struct {
			Error  string `json:"error"`
			Detail struct {
				Code string `json:"code"`
			} `json:"detail"`
		}
		_ = json.Unmarshal(data, &problem)
		code := problem.Detail.Code
		if code == "" {
			code = problem.Error
		}
		return &apiError{Status: resp.StatusCode, Code: code}
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}
