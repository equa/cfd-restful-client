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
	if err := json.Unmarshal(body, &m); err == nil {
		for _, k := range []string{"message", "error"} {
			if v, ok := m[k]; ok && v != nil {
				return fmt.Sprint(v)
			}
		}
		return fmt.Sprint(m)
	}
	s := strings.TrimSpace(string(body))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// do issues the request and applies the shared error handling: network error,
// an optional friendly 404, and the non-2xx check. On success the response is
// returned with its Body still open (for streaming); on failure the Body is
// read (for the message) and closed.
func (c *Client) do(req *http.Request, ctx, notFound string) (*http.Response, error) {
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", ctx, err)
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

// requestJSON does a request and decodes the JSON body. An empty body yields {}.
func (c *Client) requestJSON(method, u, ctx, notFound string, headers map[string]string) (any, error) {
	req, err := http.NewRequest(method, u, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: %v", ctx, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.do(req, ctx, notFound)
	if err != nil {
		return nil, err
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
	resp, err := c.do(req, "upload", "")
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
	resp, err := c.do(req, "download", "not found on server: "+remoteName)
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
func (c *Client) lsFs(name string) (any, error) {
	tail := ""
	if name != "" {
		tail = "/" + escPath(name)
	}
	return c.requestJSON("GET", c.base+"/api/rw/ls"+tail, "ls-fs", "", nil)
}

// newest returns the info of the newest transfer-area file sharing a stem.
func (c *Client) newest(stem string) (any, error) {
	return c.requestJSON("GET", c.base+"/api/rw/newest/"+url.PathEscape(stem), "newest", "", nil)
}

// lsPath lists any path within the server's CFD_HOME (/api/ls).
func (c *Client) lsPath(p string) (any, error) {
	return c.requestJSON("GET", c.base+"/api/ls/"+escPath(p), "ls",
		"path not found under CFD_HOME: "+p, nil)
}

// lsWd lists working directories under CFD_HOME, or one case's contents.
func (c *Client) lsWd(caseName string) (any, error) {
	tail := ""
	if caseName != "" {
		tail = "/" + escPath(caseName)
	}
	return c.requestJSON("GET", c.base+"/api/wd/ls"+tail, "ls-wd", "", nil)
}

// metadata returns full metadata for a working-dir case.
func (c *Client) metadata(casePath string) (any, error) {
	return c.requestJSON("GET", c.base+"/api/metadata/"+escPath(casePath), "metadata", "", nil)
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
		"not found on server: "+remoteName, nil)
}

// cleanup asks the server to purge old files.
func (c *Client) cleanup() (any, error) {
	return c.requestJSON("GET", c.base+"/api/rw/cleanup_folders", "cleanup", "", nil)
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
	notFound := fmt.Sprintf(
		"backend could not reach the uploaded file or resolve the working dir "+
			"(url=%s, wd=%s)", fileURL, wd)
	return c.requestJSON("POST", endpoint, "upstage", notFound,
		map[string]string{"Accept": "application/json"})
}

// downstage packs a processed case on the server and publishes it for download.
func (c *Client) downstage(casePath string) (any, error) {
	if casePath == "" {
		return nil, fmt.Errorf("downstage: case_path is required")
	}
	endpoint := c.base + "/api/downstage/" + escPath(casePath)
	return c.requestJSON("POST", endpoint, "downstage",
		"case not found on server: "+casePath,
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
