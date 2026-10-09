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
	"strconv"
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
	// URL is where a person settles payment_required.
	URL string
	// Message describes a rejected request (HTTP 422).
	Message string
	// RetryAfter is how many seconds to wait after rate_limited.
	RetryAfter int
}

func (e *apiError) Error() string {
	msg := fmt.Sprintf("api: %d %s", e.Status, e.Code)
	if e.Message != "" {
		msg += ": " + e.Message
	}
	return msg
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
		return parseAPIError(resp, data)
	}
	if out == nil || len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, out)
}

// parseAPIError reads the API's error forms: {"detail": {"code": …}},
// {"detail": [validation errors]} and the device flow's {"error": …}.
func parseAPIError(resp *http.Response, data []byte) *apiError {
	var problem struct {
		Error  string          `json:"error"`
		Detail json.RawMessage `json:"detail"`
	}
	_ = json.Unmarshal(data, &problem)
	e := &apiError{Status: resp.StatusCode, Code: problem.Error}
	var detail struct {
		Code       string `json:"code"`
		URL        string `json:"url"`
		RetryAfter int    `json:"retry_after"`
	}
	if json.Unmarshal(problem.Detail, &detail) == nil && detail.Code != "" {
		e.Code, e.URL, e.RetryAfter = detail.Code, detail.URL, detail.RetryAfter
	}
	var invalid []struct {
		Loc []any  `json:"loc"`
		Msg string `json:"msg"`
	}
	if resp.StatusCode == http.StatusUnprocessableEntity && json.Unmarshal(problem.Detail, &invalid) == nil && len(invalid) > 0 {
		e.Code = "invalid_request"
		loc := make([]string, 0, len(invalid[0].Loc))
		for _, part := range invalid[0].Loc {
			loc = append(loc, fmt.Sprint(part))
		}
		e.Message = strings.Join(loc, ".") + ": " + invalid[0].Msg
	}
	if e.RetryAfter == 0 {
		e.RetryAfter, _ = strconv.Atoi(resp.Header.Get("Retry-After"))
	}
	return e
}
