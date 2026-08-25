package main

import (
	"archive/zip"
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const defaultTimeout = 600 // seconds

// progressFn receives a 0..100 percentage during a transfer.
type progressFn func(pct float64)

// Client is a small HTTP client for the cfd-backend file-server endpoints.
type Client struct {
	base        string // normalised base URL, no trailing slash
	frontendURL string
	http        *http.Client
	progress    progressFn
}

// newClientFromBackend builds a Client from a resolved backend map (loadConfig).
func newClientFromBackend(backend map[string]any, progress progressFn) (*Client, error) {
	serverURL, _ := backend["server_url"].(string)
	base, err := normaliseURL(serverURL)
	if err != nil {
		return nil, err
	}

	timeout := float64(defaultTimeout)
	if v, ok := backend["timeout"].(float64); ok {
		timeout = v
	}
	verifyTLS := true
	if v, ok := backend["verify_tls"].(bool); ok {
		verifyTLS = v
	}
	frontend := base
	if v, ok := backend["frontend_url"].(string); ok && v != "" {
		if f, err := normaliseURL(v); err == nil {
			frontend = f
		}
	}

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: !verifyTLS},
	}
	return &Client{
		base:        base,
		frontendURL: frontend,
		http:        &http.Client{Timeout: time.Duration(timeout) * time.Second, Transport: tr},
		progress:    progress,
	}, nil
}

// normaliseURL returns a clean base URL: add a scheme if missing, strip a
// trailing '/', drop any query/fragment.
func normaliseURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("server_url is empty")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid server_url %q: %v", raw, err)
	}
	return u.Scheme + "://" + u.Host + strings.TrimRight(u.Path, "/"), nil
}

// escPath escapes each path segment but keeps '/' (matches Python quote(safe='/')).
func escPath(s string) string {
	parts := strings.Split(s, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

func (c *Client) rw(name string) string { return c.base + "/api/rw/" + escPath(name) }

// -- request plumbing ------------------------------------------------------- //

func serverMessage(body []byte) string {
	var m map[string]any
	if json.Unmarshal(body, &m) == nil {
		for _, k := range []string{"message", "error"} {
			if v, ok := m[k]; ok && v != nil {
				return fmt.Sprint(v)
			}
		}
	}
	// No message/error field (or non-JSON body): fall back to the raw text,
	// capped -- cleaner than Go's map formatting for a bare {} body.
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// do issues the request and applies the shared error handling: network error,
// an optional friendly 404, and the non-2xx check. On success the response is
// returned with its Body still open (for streaming). When empty404 is set, a
// 404 is not an error -- do returns (nil, nil) so a lookup can report "not
// found" as an empty result rather than a failure (the query itself succeeded).
func (c *Client) do(req *http.Request, ctx, notFound string, empty404 bool) (*http.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", ctx, err)
	}
	if empty404 && resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, nil
	}
	if notFound != "" && resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return nil, fmt.Errorf("%s: %s", ctx, notFound)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("%s: HTTP %d %s", ctx, resp.StatusCode, serverMessage(b))
	}
	return resp, nil
}

// requestJSON does a request and decodes the JSON body. An empty body -- or, if
// empty404 is set, a 404 -- yields {}.
func (c *Client) requestJSON(method, u, ctx, notFound string, empty404 bool, headers map[string]string) (any, error) {
	req, err := http.NewRequest(method, u, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", ctx, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.do(req, ctx, notFound, empty404)
	if err != nil {
		return nil, err
	}
	if resp == nil { // empty404: not found -> empty result
		return map[string]any{}, nil
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", ctx, err)
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return map[string]any{}, nil
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("%s: invalid JSON in response: %v", ctx, err)
	}
	return out, nil
}

// orEmpty maps a null/absent JSON result to {} so a "list a specific item" miss
// reads consistently across ls / ls-fs / ls-wd (some backends return null).
func orEmpty(v any) any {
	if v == nil {
		return map[string]any{}
	}
	return v
}

// -- operations ------------------------------------------------------------- //

// upload uploads a file, or a directory (zipped first), as the raw request body.
func (c *Client) upload(localPath, remoteName string) (any, error) {
	fi, err := os.Stat(localPath)
	if err != nil {
		return nil, fmt.Errorf("upload: local path not found: %s", localPath)
	}
	if fi.IsDir() {
		return c.uploadDir(localPath, remoteName)
	}
	if remoteName == "" {
		remoteName = filepath.Base(localPath)
	}
	return c.uploadFile(localPath, remoteName)
}

func (c *Client) uploadFile(localPath, remoteName string) (any, error) {
	fi, err := os.Stat(localPath)
	if err != nil {
		return nil, fmt.Errorf("upload: local path not found: %s", localPath)
	}
	size := fi.Size()
	if size == 0 {
		// The server ignores a zero-length body (content_length > 0 guard).
		return nil, fmt.Errorf("upload: refusing to send empty file %s", localPath)
	}
	fh, err := os.Open(localPath)
	if err != nil {
		return nil, fmt.Errorf("upload: %v", err)
	}
	defer fh.Close()

	if c.progress != nil {
		c.progress(0)
	}
	var body io.Reader = fh
	if c.progress != nil {
		body = &progressReader{r: fh, total: size, progress: c.progress}
	}
	req, err := http.NewRequest("POST", c.rw(remoteName), body)
	if err != nil {
		return nil, fmt.Errorf("upload: %v", err)
	}
	// Set ContentLength explicitly so requests-style raw-body upload sends a
	// Content-Length (which the server requires) rather than chunked encoding.
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.do(req, "upload", "", false)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	if c.progress != nil {
		c.progress(100)
	}
	return map[string]any{"uploaded": remoteName, "bytes": size, "source": localPath}, nil
}

func (c *Client) uploadDir(dirPath, remoteName string) (any, error) {
	if remoteName == "" {
		remoteName = filepath.Base(dirPath) + ".zip"
	}
	var files []string
	err := filepath.Walk(dirPath, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("upload: %v", err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("upload: directory has no files to zip: %s", dirPath)
	}
	sort.Strings(files)

	tmp, err := os.CreateTemp("", "ida_cfd_*.zip")
	if err != nil {
		return nil, fmt.Errorf("upload: %v", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	zw := zip.NewWriter(tmp)
	for _, f := range files {
		rel, err := filepath.Rel(dirPath, f)
		if err != nil {
			zw.Close()
			tmp.Close()
			return nil, fmt.Errorf("upload: %v", err)
		}
		// contents sit at the zip root; use forward slashes in the archive
		w, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			zw.Close()
			tmp.Close()
			return nil, fmt.Errorf("upload: %v", err)
		}
		in, err := os.Open(f)
		if err != nil {
			zw.Close()
			tmp.Close()
			return nil, fmt.Errorf("upload: %v", err)
		}
		if _, err := io.Copy(w, in); err != nil {
			in.Close()
			zw.Close()
			tmp.Close()
			return nil, fmt.Errorf("upload: %v", err)
		}
		in.Close()
	}
	if err := zw.Close(); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("upload: %v", err)
	}
	tmp.Close()

	res, err := c.uploadFile(tmpName, remoteName)
	if err != nil {
		return nil, err
	}
	m := res.(map[string]any)
	m["source"] = dirPath
	m["zipped"] = true
	m["files"] = len(files)
	return m, nil
}

// download downloads a file to localDest (a dir joins the remote name). Writes
// to a .part temp then atomically renames.
func (c *Client) download(remoteName, localDest string) (any, error) {
	// If the destination is an existing directory or ends with a separator,
	// join it with the remote name.
	if fi, err := os.Stat(localDest); (err == nil && fi.IsDir()) ||
		strings.HasSuffix(localDest, "/") || strings.HasSuffix(localDest, string(os.PathSeparator)) {
		localDest = filepath.Join(localDest, remoteName)
	}

	req, err := http.NewRequest("GET", c.rw(remoteName), nil)
	if err != nil {
		return nil, fmt.Errorf("download: %v", err)
	}
	resp, err := c.do(req, "download", "not found on server: "+remoteName, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	total := resp.ContentLength // -1 if unknown
	if c.progress != nil {
		c.progress(0)
	}
	if dir := filepath.Dir(localDest); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("download: %v", err)
		}
	}
	tmp := localDest + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return nil, fmt.Errorf("download: %v", err)
	}
	var reader io.Reader = resp.Body
	if c.progress != nil && total > 0 {
		reader = &progressReader{r: resp.Body, total: total, progress: c.progress}
	}
	n, err := io.Copy(out, reader)
	out.Close()
	if err != nil {
		os.Remove(tmp)
		return nil, fmt.Errorf("download: %v", err)
	}
	if err := os.Rename(tmp, localDest); err != nil {
		os.Remove(tmp)
		return nil, fmt.Errorf("download: %v", err)
	}
	if c.progress != nil {
		c.progress(100)
	}
	return map[string]any{"downloaded": remoteName, "path": localDest, "bytes": n}, nil
}

// lsFs lists the flat upload/transfer area (/api/rw), or one file's info.
// A missing name yields {} (server returns it; normalised for safety).
func (c *Client) lsFs(name string) (any, error) {
	tail := ""
	if name != "" {
		tail = "/" + escPath(name)
	}
	res, err := c.requestJSON("GET", c.base+"/api/rw/ls"+tail, "ls-fs", "", false, nil)
	if err != nil {
		return nil, err
	}
	return orEmpty(res), nil
}

// newest returns the info of the newest transfer-area file sharing a stem.
func (c *Client) newest(stem string) (any, error) {
	return c.requestJSON("GET", c.base+"/api/rw/newest/"+url.PathEscape(stem), "newest", "", false, nil)
}

// lsPath lists any path within the server's CFD_HOME (/api/ls). A missing path
// (the server 404s -- the file browser relies on that) is reported as an empty
// result with success: the lookup ran, the answer is "nothing there".
func (c *Client) lsPath(p string) (any, error) {
	res, err := c.requestJSON("GET", c.base+"/api/ls/"+escPath(p), "ls", "", true, nil)
	if err != nil {
		return nil, err
	}
	return orEmpty(res), nil
}

// lsWd lists working directories under CFD_HOME, or one case's contents.
// A missing case yields {} (normalised in case a backend returns null).
func (c *Client) lsWd(caseName string) (any, error) {
	tail := ""
	if caseName != "" {
		tail = "/" + escPath(caseName)
	}
	res, err := c.requestJSON("GET", c.base+"/api/wd/ls"+tail, "ls-wd", "", false, nil)
	if err != nil {
		return nil, err
	}
	return orEmpty(res), nil
}

// metadata returns full metadata for a working-dir case.
func (c *Client) metadata(casePath string) (any, error) {
	return c.requestJSON("GET", c.base+"/api/metadata/"+escPath(casePath), "metadata", "", false, nil)
}

// exists reports whether a transfer-area file exists (lsFs returns {} if absent).
func (c *Client) exists(remoteName string) (bool, error) {
	res, err := c.lsFs(remoteName)
	if err != nil {
		return false, err
	}
	return truthy(res), nil
}

// delete removes a transfer-area file.
func (c *Client) delete(remoteName string) (any, error) {
	return c.requestJSON("DELETE", c.rw(remoteName), "delete",
		"not found on server: "+remoteName, false, nil)
}

// cleanup asks the server to purge old files.
func (c *Client) cleanup() (any, error) {
	return c.requestJSON("GET", c.base+"/api/rw/cleanup_folders", "cleanup", "", false, nil)
}

// upstage stages an already-uploaded file into a backend working directory.
func (c *Client) upstage(remoteName, wd string) (any, error) {
	if wd == "" {
		wd = strings.TrimSuffix(filepath.Base(remoteName), filepath.Ext(remoteName))
	}
	if wd == "" {
		return nil, fmt.Errorf("upstage: --wd is required")
	}
	fileURL := c.rw(remoteName)
	q := url.Values{}
	q.Set("wd", wd)
	q.Set("url", fileURL)
	endpoint := c.base + "/api/upstage/" + escPath(wd) + "?" + q.Encode()
	// No notFound override: the backend now distinguishes "file not found on the
	// server" (404, usually a wrong/missing name) from "file server unreachable"
	// (502), each with a specific message -- relay it verbatim rather than
	// collapsing both into one blanket "could not reach" string.
	return c.requestJSON("POST", endpoint, "upstage", "", false,
		map[string]string{"Accept": "application/json"})
}

// ensure asks the backend to make case_id usable in CFD_HOME with the least
// work (the resume entry point). The backend replies (as the "result"):
//
//	{"status":"ready", "state":...}  -- staged and usable
//	{"status":"need_upload"}         -- caller should upload the case, then retry
//
// force=true re-stages from the file-server upload even if a copy is staged
// (clean replace) -- for when a different backend holds the newer copy.
func (c *Client) ensure(caseID string, force bool) (any, error) {
	if caseID == "" {
		return nil, fmt.Errorf("ensure: case-id is required")
	}
	endpoint := c.base + "/api/ensure/" + escPath(caseID)
	if force {
		endpoint += "?force=1"
	}
	return c.requestJSON("POST", endpoint, "ensure", "", false,
		map[string]string{"Accept": "application/json"})
}

// save asks the backend for the newest downloadable archive of a case -- the
// save entry point, mirror of ensure. `since` is the archive mtime recorded on
// the last save (0 to force a check). The backend replies (as "result"):
//
//	{"status":"up_to_date"}                                   -- keep the local save
//	{"status":"ready","url":...,"name":...,"mtime":...,"published":bool}
//
// On "ready", record `mtime` (pass it back as `since` next time) and download
// the archive by `name`. The backend publishes a fresh <case>.downstage only
// when the staged case is newer than any existing archive.
func (c *Client) save(caseID string, since float64) (any, error) {
	if caseID == "" {
		return nil, fmt.Errorf("save: case-id is required")
	}
	endpoint := c.base + "/api/save/" + escPath(caseID)
	if since > 0 {
		endpoint += "?since=" + strconv.FormatFloat(since, 'f', -1, 64)
	}
	return c.requestJSON("POST", endpoint, "save", "", false,
		map[string]string{"Accept": "application/json"})
}

// downstage packs a processed case on the server and publishes it for download.
func (c *Client) downstage(casePath string) (any, error) {
	if casePath == "" {
		return nil, fmt.Errorf("downstage: case_path is required")
	}
	endpoint := c.base + "/api/downstage/" + escPath(casePath)
	return c.requestJSON("POST", endpoint, "downstage",
		"case not found on server: "+casePath, false,
		map[string]string{"Accept": "application/json"})
}

// -- helpers ---------------------------------------------------------------- //

// progressReader forwards each read's running total to a progress callback.
type progressReader struct {
	r        io.Reader
	total    int64
	read     int64
	progress progressFn
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.read += int64(n)
		if p.total > 0 && p.progress != nil {
			p.progress(float64(p.read) * 100 / float64(p.total))
		}
	}
	return n, err
}

// truthy mirrors Python's bool() over a decoded JSON value.
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case map[string]any:
		return len(x) > 0
	case []any:
		return len(x) > 0
	default:
		return true
	}
}
