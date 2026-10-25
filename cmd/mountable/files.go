package main

// The files commands: the files API without mounting, through a token
// session that lives for one command.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// filesSessionSeconds is the token session's maximum duration.
const filesSessionSeconds = 3600

const (
	filesWriteUsage   = "files write FS_ID PATH [--from FILE] [--if-match ETAG] [--if-none-match]"
	filesReadUsage    = "files read FS_ID PATH [--to FILE]"
	filesDeleteUsage  = "files delete FS_ID PATH [--if-match ETAG]"
	filesListUsage    = "files list FS_ID [PATH] [--after NAME] [--limit N]"
	filesChangesUsage = "files changes FS_ID [--cursor CURSOR] [--wait SECONDS]"
)

// fileInfo is the files API's FileInfo.
type fileInfo struct {
	Path       string `json:"path"`
	Type       string `json:"type"`
	Size       int64  `json:"size"`
	ETag       string `json:"etag,omitempty"`
	ModifiedAt string `json:"modified_at"`
}

func filesWrite(o *output, args []string) error {
	f := newFlags(filesWriteUsage)
	from := f.String("from", "", "the file to send; stdin when not given")
	ifMatch := f.String("if-match", "", "write only if the file's ETag is this")
	ifNoneMatch := f.Bool("if-none-match", false, "write only if nothing exists at PATH")
	positional, err := f.parse(args, 2)
	if err != nil {
		return err
	}
	path, err := filePath(positional[1])
	if err != nil {
		return err
	}
	// A regular file is streamed with its size; stdin, a pipe or an empty
	// file is read fully first, so Content-Length is known.
	var body io.Reader = os.Stdin
	size := int64(-1)
	if *from != "" {
		file, err := os.Open(*from)
		if err != nil {
			return err
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil {
			return err
		}
		body = file
		if info.Mode().IsRegular() && info.Size() > 0 {
			size = info.Size()
		}
	}
	if size < 0 {
		data, err := io.ReadAll(body)
		if err != nil {
			return err
		}
		body, size = bytes.NewReader(data), int64(len(data))
	}
	return withFiles(o, positional[0], "rw", func(ctx context.Context, s *filesSession) error {
		req, err := s.request(ctx, "PUT", "/files/"+path, body)
		if err != nil {
			return err
		}
		req.ContentLength = size
		req.Header.Set("Content-Type", "application/octet-stream")
		if *ifMatch != "" {
			req.Header.Set("If-Match", quoteETag(*ifMatch))
		}
		if *ifNoneMatch {
			req.Header.Set("If-None-Match", "*")
		}
		raw, err := s.json(req, 0)
		if err != nil {
			return err
		}
		return o.result(raw, func(w io.Writer) error {
			var info fileInfo
			if err := json.Unmarshal(raw, &info); err != nil {
				return err
			}
			return printFileInfo(w, info)
		})
	})
}

// filesRead streams the file to stdout, or with --to replaces FILE once
// the whole file arrived.
func filesRead(o *output, args []string) error {
	f := newFlags(filesReadUsage)
	to := f.String("to", "", "the file to write; stdout when not given")
	positional, err := f.parse(args, 2)
	if err != nil {
		return err
	}
	if o.json && *to == "" {
		e := usageError(filesReadUsage)
		e.Message = "files read --json needs --to FILE: the file's bytes are not JSON"
		return e
	}
	path, err := filePath(positional[1])
	if err != nil {
		return err
	}
	return withFiles(o, positional[0], "ro", func(ctx context.Context, s *filesSession) error {
		req, err := s.request(ctx, "GET", "/files/"+path, nil)
		if err != nil {
			return err
		}
		resp, err := s.do(req, 0)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if *to == "" {
			n, err := io.Copy(o.stdout, resp.Body)
			return brokenOff(err, n, "the output is incomplete")
		}
		n, err := saveFile(*to, resp.Body)
		if err != nil {
			return err
		}
		info := fileInfo{Path: "/" + strings.Trim(positional[1], "/"), Type: "file", Size: n, ETag: resp.Header.Get("ETag")}
		if modified, err := http.ParseTime(resp.Header.Get("Last-Modified")); err == nil {
			info.ModifiedAt = modified.UTC().Format(time.RFC3339)
		}
		return o.result(info, func(w io.Writer) error { return printFileInfo(w, info) })
	})
}

func filesDelete(o *output, args []string) error {
	f := newFlags(filesDeleteUsage)
	ifMatch := f.String("if-match", "", "delete only if the file's ETag is this")
	positional, err := f.parse(args, 2)
	if err != nil {
		return err
	}
	path, err := filePath(positional[1])
	if err != nil {
		return err
	}
	return withFiles(o, positional[0], "rw", func(ctx context.Context, s *filesSession) error {
		req, err := s.request(ctx, "DELETE", "/files/"+path, nil)
		if err != nil {
			return err
		}
		if *ifMatch != "" {
			req.Header.Set("If-Match", quoteETag(*ifMatch))
		}
		if _, err := s.json(req, 0); err != nil {
			return err
		}
		deleted := "/" + strings.Trim(positional[1], "/")
		return o.result(map[string]any{"path": deleted, "deleted": true}, func(w io.Writer) error {
			_, err := fmt.Fprintf(w, "deleted %s\n", deleted)
			return err
		})
	})
}

// filesList lists one page; a stderr note gives the --after for the next.
func filesList(o *output, args []string) error {
	f := newFlags(filesListUsage)
	after := f.String("after", "", "list the entries after this name")
	limit := f.Uint64("limit", 0, "at most this many entries")
	positional, err := f.parse(args, -1)
	if err != nil {
		return err
	}
	if len(positional) < 1 || len(positional) > 2 {
		return usageError(filesListUsage)
	}
	path := ""
	if len(positional) == 2 {
		if path, err = filePath(positional[1]); err != nil {
			return err
		}
	}
	given := f.given()
	query := url.Values{}
	if given["after"] {
		query.Set("after", *after)
	}
	if given["limit"] {
		query.Set("limit", strconv.FormatUint(*limit, 10))
	}
	return withFiles(o, positional[0], "ro", func(ctx context.Context, s *filesSession) error {
		req, err := s.request(ctx, "GET", "/list/"+path+queryString(query), nil)
		if err != nil {
			return err
		}
		raw, err := s.json(req, 0)
		if err != nil {
			return err
		}
		return o.result(raw, func(w io.Writer) error {
			var page struct {
				Entries []fileInfo `json:"entries"`
				Next    *string    `json:"next"`
			}
			if err := json.Unmarshal(raw, &page); err != nil {
				return err
			}
			t := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
			fmt.Fprintln(t, "TYPE\tSIZE\tMODIFIED\tPATH")
			for _, e := range page.Entries {
				fmt.Fprintf(t, "%s\t%d\t%s\t%s\n", e.Type, e.Size, e.ModifiedAt, e.Path)
			}
			if err := t.Flush(); err != nil {
				return err
			}
			if page.Next != nil {
				note(o.stderr, "more entries: pass --after %s for the next page", *page.Next)
			}
			return nil
		})
	})
}

// filesChanges asks for changes once. Without --cursor the API answers at
// once with the current cursor; with one, it waits up to --wait seconds
// (default 25) for a change. wait is sent with a cursor, or when given.
func filesChanges(o *output, args []string) error {
	f := newFlags(filesChangesUsage)
	cursor := f.String("cursor", "", "the cursor of the last changes seen")
	wait := f.Uint64("wait", 25, "seconds to wait for a change")
	positional, err := f.parse(args, 1)
	if err != nil {
		return err
	}
	given := f.given()
	query := url.Values{}
	if given["cursor"] {
		query.Set("cursor", *cursor)
	}
	var waited time.Duration
	if given["cursor"] || given["wait"] {
		query.Set("wait", strconv.FormatUint(*wait, 10))
		waited = time.Duration(*wait) * time.Second
	}
	return withFiles(o, positional[0], "ro", func(ctx context.Context, s *filesSession) error {
		req, err := s.request(ctx, "GET", "/changes"+queryString(query), nil)
		if err != nil {
			return err
		}
		raw, err := s.json(req, waited)
		if err != nil {
			return err
		}
		return o.result(raw, func(w io.Writer) error {
			var batch struct {
				Events []struct {
					Type string `json:"type"`
					Path string `json:"path"`
				} `json:"events"`
				Cursor string `json:"cursor"`
			}
			if err := json.Unmarshal(raw, &batch); err != nil {
				return err
			}
			for _, e := range batch.Events {
				if _, err := fmt.Fprintf(w, "%s %s\n", e.Type, e.Path); err != nil {
					return err
				}
			}
			note(o.stderr, "cursor %s (pass --cursor %s for the next changes)", batch.Cursor, batch.Cursor)
			return nil
		})
	})
}

func printFileInfo(w io.Writer, info fileInfo) error {
	_, err := fmt.Fprintf(w, "%s %s %d\n", info.Path, info.ETag, info.Size)
	return err
}

// filesSession is a token session on one filesystem; its token stays in
// memory.
type filesSession struct {
	fsID  string
	token string
}

// withFiles creates a token session of mode on fsID with the login or the
// API key, runs fn with it, then revokes the session. A failed revocation
// is only noted: the session expires within the hour.
func withFiles(o *output, fsID, mode string, fn func(ctx context.Context, s *filesSession) error) error {
	c, err := newClient("")
	if err != nil {
		return err
	}
	ctx := context.Background()
	var created struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	body := map[string]any{
		"filesystem_id":        fsID,
		"mode":                 mode,
		"max_duration_seconds": filesSessionSeconds,
		"idempotency_key":      randomHex(16),
		"credential":           "token",
	}
	if err := c.call(ctx, "POST", "/api/v1/mount-sessions", body, &created); err != nil {
		return err
	}
	if created.ID != "" {
		defer func() {
			if err := c.revokeSession(ctx, created.ID); err != nil {
				note(o.stderr, "could not revoke token session %s (%s); it expires within an hour", created.ID, toCLIError(err).Code)
			}
		}()
	}
	if created.ID == "" || created.Token == "" {
		return &cliError{Code: "api_error", Message: "the API returned no session token", Hint: "retry later"}
	}
	return fn(ctx, &filesSession{fsID: fsID, token: created.Token})
}

// request builds a call to path under the filesystem, with the session's
// token.
func (s *filesSession) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, apiURL()+"/api/v1/filesystems/"+url.PathEscape(s.fsID)+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.token)
	return req, nil
}

// do sends req, allowing extra time beyond the usual timeout (a change
// poll's wait). An answer other than 2xx is the error; otherwise the caller
// closes the answer's body.
func (s *filesSession) do(req *http.Request, extra time.Duration) (*http.Response, error) {
	hc := *httpClient
	hc.Timeout += extra
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(resp.Body)
		e := parseAPIError(resp, data)
		if e.Code == "not_found" {
			return nil, &cliError{Code: e.Code, Message: e.Message, Hint: "check the ID with `mountable fs list` and the path with `mountable files list FS_ID`"}
		}
		return nil, e
	}
	return resp, nil
}

// json sends req and returns its JSON answer (nil when empty).
func (s *filesSession) json(req *http.Request, extra time.Duration) (json.RawMessage, error) {
	resp, err := s.do(req, extra)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return nil, nil
	}
	if !json.Valid(data) {
		return nil, &cliError{Code: "api_error", Message: "the API's answer is not JSON", Hint: "retry later"}
	}
	return data, nil
}

// filePath is path as the files API takes it: each segment percent-encoded,
// leading and trailing "/" dropped. A "." or ".." segment is refused before
// any request, since URL normalisation would resolve it.
func filePath(path string) (string, error) {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return "", nil
	}
	segments := strings.Split(trimmed, "/")
	for i, segment := range segments {
		if segment == "." || segment == ".." {
			return "", &cliError{Code: "bad_path", Message: fmt.Sprintf("invalid path %q", path)}
		}
		segments[i] = escapeSegment(segment)
	}
	return strings.Join(segments, "/"), nil
}

// escapeSegment percent-encodes every byte but A–Z, a–z, 0–9 and -._~
// (url.PathEscape keeps more, such as ! and @).
func escapeSegment(segment string) string {
	var b strings.Builder
	for i := 0; i < len(segment); i++ {
		c := segment[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || strings.IndexByte("-._~", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func queryString(query url.Values) string {
	if len(query) == 0 {
		return ""
	}
	return "?" + query.Encode()
}

// quoteETag quotes an ETag unless it already is (or is weak).
func quoteETag(etag string) string {
	if strings.HasPrefix(etag, `"`) || strings.HasPrefix(etag, "W/") {
		return etag
	}
	return `"` + etag + `"`
}

// saveFile writes r to name through a temporary file in the same
// directory, so name is replaced only once all of r arrived.
func saveFile(name string, r io.Reader) (int64, error) {
	tmp, err := os.CreateTemp(filepath.Dir(name), "."+filepath.Base(name)+".*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name()) // nothing left to remove after the rename
	n, err := io.Copy(tmp, r)
	if err := brokenOff(err, n, name+" was not changed"); err != nil {
		tmp.Close()
		return n, err
	}
	if err := tmp.Close(); err != nil {
		return n, err
	}
	return n, os.Rename(tmp.Name(), name)
}

// brokenOff reports a copy of the file's body that stopped after n bytes. A
// local write error stays as it is; anything else is the download breaking
// off.
func brokenOff(err error, n int64, consequence string) error {
	var local *fs.PathError
	if err == nil || errors.As(err, &local) {
		return err
	}
	return &cliError{Code: "network_error", Message: fmt.Sprintf("the file broke off after %d bytes (%v); %s", n, err, consequence)}
}
