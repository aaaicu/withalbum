//go:build unix

// Package harness runs the real withalbum-server binary as a child process
// and talks to it over its HTTP API. It is shared by the developer tools in
// tools/ (chaos test, performance measurement) and is never part of the
// server itself.
//
// Every child gets its own WITHALBUM_CONFIG, so the per-user config file of
// the machine running the tool is never read or written.
package harness

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Server is one withalbum-server child process on a loopback port.
type Server struct {
	Bin    string   // path of the withalbum-server binary
	Root   string   // WITHALBUM_ROOT; "" uses the config file only
	Dir    string   // working directory of the child (default: the tool's)
	Config string   // WITHALBUM_CONFIG (a file in a scratch directory)
	Listen string   // 127.0.0.1:port
	Env    []string // extra KEY=VALUE pairs
	// Log receives the child's stderr (and stdout) lines when non-nil.
	Log io.Writer

	mu    sync.Mutex
	cmd   *exec.Cmd
	done  chan struct{}
	lines []string
	wait  error
	usage *syscall.Rusage
}

// FreePort returns a loopback address with a currently unused port.
func FreePort() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer ln.Close()
	return ln.Addr().String(), nil
}

func (s *Server) env() []string {
	env := []string{
		"PATH=/usr/bin:/bin:/usr/sbin:/sbin",
		"HOME=" + os.Getenv("HOME"),
		"WITHALBUM_CONFIG=" + s.Config,
		"WITHALBUM_LISTEN=" + s.Listen,
	}
	if s.Root != "" {
		env = append(env, "WITHALBUM_ROOT="+s.Root)
	}
	return append(env, s.Env...)
}

// Command runs a console subcommand (verify, admin, backup, ...) to the end
// and returns its exit code, combined output and resource usage.
func (s *Server) Command(stdin string, args ...string) (int, string, *syscall.Rusage, error) {
	cmd := exec.Command(s.Bin, args...)
	cmd.Env, cmd.Dir = s.env(), s.Dir
	cmd.Stdin = strings.NewReader(stdin)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	var ru *syscall.Rusage
	if cmd.ProcessState != nil {
		ru, _ = cmd.ProcessState.SysUsage().(*syscall.Rusage)
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), out.String(), ru, nil
	}
	if err != nil {
		return -1, out.String(), ru, err
	}
	return 0, out.String(), ru, nil
}

// Start launches the server and waits until /api/v1/health answers or the
// process exits (an error that includes its last output).
func (s *Server) Start(timeout time.Duration) error { return s.StartWith(nil, timeout) }

// StartWith is Start with extra KEY=VALUE pairs for this run only.
func (s *Server) StartWith(extra []string, timeout time.Duration) error {
	s.mu.Lock()
	if s.cmd != nil {
		s.mu.Unlock()
		return errors.New("harness: server already running")
	}
	cmd := exec.Command(s.Bin)
	cmd.Env, cmd.Dir = append(s.env(), extra...), s.Dir
	pr, pw, err := os.Pipe()
	if err != nil {
		s.mu.Unlock()
		return err
	}
	cmd.Stdout, cmd.Stderr = pw, pw
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		s.mu.Unlock()
		return err
	}
	pw.Close()
	s.cmd, s.done, s.lines, s.wait, s.usage = cmd, make(chan struct{}), nil, nil, nil
	done := s.done
	s.mu.Unlock()
	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			s.mu.Lock()
			s.lines = append(s.lines, line)
			if len(s.lines) > 2000 {
				s.lines = s.lines[len(s.lines)-1000:]
			}
			s.mu.Unlock()
			if s.Log != nil {
				fmt.Fprintln(s.Log, line)
			}
		}
		pr.Close()
		err := cmd.Wait()
		s.mu.Lock()
		s.wait = err
		if cmd.ProcessState != nil {
			s.usage, _ = cmd.ProcessState.SysUsage().(*syscall.Rusage)
		}
		s.mu.Unlock()
		close(done)
	}()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			s.mu.Lock()
			s.cmd = nil
			s.mu.Unlock()
			return fmt.Errorf("harness: server exited during startup (%v): %s", s.wait, s.Tail(8))
		default:
		}
		if _, err := s.Health(); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.Kill()
	return fmt.Errorf("harness: server did not answer within %s: %s", timeout, s.Tail(8))
}

// PID is the running child's process id, or 0.
func (s *Server) PID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cmd == nil || s.cmd.Process == nil {
		return 0
	}
	return s.cmd.Process.Pid
}

// Running reports whether the child has not exited yet.
func (s *Server) Running() bool {
	s.mu.Lock()
	cmd, done := s.cmd, s.done
	s.mu.Unlock()
	if cmd == nil {
		return false
	}
	select {
	case <-done:
		return false
	default:
		return true
	}
}

// Tail returns the last n output lines.
func (s *Server) Tail(n int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	from := len(s.lines) - n
	if from < 0 {
		from = 0
	}
	return strings.Join(s.lines[from:], " | ")
}

// WaitLine waits until an output line contains substr.
func (s *Server) WaitLine(substr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		for _, l := range s.lines {
			if strings.Contains(l, substr) {
				s.mu.Unlock()
				return true
			}
		}
		s.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	return false
}

// Kill sends SIGKILL and waits for the exit.
func (s *Server) Kill() { s.signal(syscall.SIGKILL) }

// Stop sends SIGTERM (graceful shutdown) and waits; it kills after 15s.
func (s *Server) Stop() { s.signal(syscall.SIGTERM) }

func (s *Server) signal(sig syscall.Signal) {
	s.mu.Lock()
	cmd, done := s.cmd, s.done
	s.mu.Unlock()
	if cmd == nil {
		return
	}
	_ = cmd.Process.Signal(sig)
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		<-done
	}
	s.mu.Lock()
	s.cmd = nil
	s.mu.Unlock()
}

// MaxRSS is the peak resident set size in bytes of the last exited child.
func (s *Server) MaxRSS() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return RSSBytes(s.usage)
}

// Exited reports whether the child exited by itself (not via Kill/Stop)
// and its wait error.
func (s *Server) Exited() (bool, error) {
	s.mu.Lock()
	cmd, done := s.cmd, s.done
	s.mu.Unlock()
	if cmd == nil || done == nil {
		return false, nil
	}
	select {
	case <-done:
		s.mu.Lock()
		defer s.mu.Unlock()
		s.cmd = nil
		return true, s.wait
	default:
		return false, nil
	}
}

func (s *Server) base() string { return "http://" + s.Listen }

// Health returns the storage state ("ok"/"unavailable") of /api/v1/health.
func (s *Server) Health() (string, error) {
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(s.base() + "/api/v1/health")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var h struct {
		Storage string `json:"storage"`
	}
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("health: %s", resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil {
		return "", err
	}
	return h.Storage, nil
}

// Client is a logged-in API session.
type Client struct {
	Base  string
	Token string
	HTTP  *http.Client
}

// Login signs in and returns a client for this server.
func (s *Server) Login(login, pass string) (*Client, error) {
	c := &Client{Base: s.base(), HTTP: &http.Client{Timeout: 60 * time.Second}}
	body, _ := json.Marshal(map[string]string{"login_name": login, "password": pass, "device_name": "harness", "platform": "macos"})
	var out struct {
		AccessToken string `json:"access_token"`
	}
	st, raw, err := c.do("POST", "/api/v1/auth/login", "application/json", bytes.NewReader(body), nil, &out)
	if err != nil {
		return nil, err
	}
	if st != 200 || out.AccessToken == "" {
		return nil, fmt.Errorf("login: %d %s", st, raw)
	}
	c.Token = out.AccessToken
	return c, nil
}

func (c *Client) do(method, path, ctype string, body io.Reader, hdr map[string]string, out any) (int, string, error) {
	req, err := http.NewRequest(method, c.Base+path, body)
	if err != nil {
		return 0, "", err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return resp.StatusCode, "", err
	}
	if out != nil && resp.StatusCode/100 == 2 {
		if err := json.Unmarshal(raw, out); err != nil {
			return resp.StatusCode, string(raw), err
		}
	}
	return resp.StatusCode, string(raw), nil
}

// GetJSON sends a GET and decodes a 2xx JSON body into out.
func (c *Client) GetJSON(path string, out any) (int, string, error) {
	return c.do("GET", path, "", nil, nil, out)
}

// CreateFolder creates a folder and returns its id.
func (c *Client) CreateFolder(name string) (string, error) {
	body, _ := json.Marshal(map[string]string{"name": name})
	var out struct {
		ID string `json:"id"`
	}
	st, raw, err := c.do("POST", "/api/v1/folders", "application/json", bytes.NewReader(body), nil, &out)
	if err != nil {
		return "", err
	}
	if st != 201 {
		return "", fmt.Errorf("create folder: %d %s", st, raw)
	}
	return out.ID, nil
}

// UploadResult is the outcome of one upload request.
type UploadResult struct {
	Status int    // 0: no HTTP response (connection error)
	Code   string // error code or result of the JSON body
	SHA256 string
	Err    error
}

// Upload sends one original. body may be a slow reader; the request has
// no Content-Length when size < 0.
func (c *Client) Upload(folderID, key, filename string, body io.Reader, size int64) UploadResult {
	req, err := http.NewRequest("POST", c.Base+"/api/v1/folders/"+url.PathEscape(folderID)+"/assets", body)
	if err != nil {
		return UploadResult{Err: err}
	}
	req.ContentLength = size
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Idempotency-Key", key)
	req.Header.Set("X-Filename", filename)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return UploadResult{Err: err}
	}
	defer resp.Body.Close()
	var out struct {
		Result string `json:"result"`
		Error  string `json:"error"`
		SHA256 string `json:"sha256"`
	}
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if rerr != nil {
		return UploadResult{Status: resp.StatusCode, Err: rerr}
	}
	_ = json.Unmarshal(raw, &out)
	code := out.Result
	if code == "" {
		code = out.Error
	}
	return UploadResult{Status: resp.StatusCode, Code: code, SHA256: out.SHA256}
}

// Asset is the part of an asset the tools compare.
type Asset struct {
	ID     string `json:"id"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// ListAssets pages through GET /folders/{id}/assets and returns every asset
// and the feed cursor of the first page.
func (c *Client) ListAssets(folderID string, limit int) ([]Asset, string, int, error) {
	var all []Asset
	after, cursor, pages := "", "", 0
	for {
		q := fmt.Sprintf("?limit=%d", limit)
		if after != "" {
			q += "&after=" + url.QueryEscape(after)
		}
		var page struct {
			Assets    []Asset `json:"assets"`
			HasMore   bool    `json:"has_more"`
			NextAfter string  `json:"next_after"`
			Cursor    string  `json:"cursor"`
		}
		st, raw, err := c.do("GET", "/api/v1/folders/"+url.PathEscape(folderID)+"/assets"+q, "", nil, nil, &page)
		if err != nil {
			return nil, "", pages, err
		}
		if st != 200 {
			return nil, "", pages, fmt.Errorf("list assets: %d %s", st, raw)
		}
		pages++
		if after == "" {
			cursor = page.Cursor
		}
		all = append(all, page.Assets...)
		if !page.HasMore {
			return all, cursor, pages, nil
		}
		after = page.NextAfter
	}
}

// ListEvents pages through GET /folders/{id}/sync from cursor ("" = the
// beginning) and returns the number of events and pages.
func (c *Client) ListEvents(folderID, cursor string, limit int) (int, int, error) {
	n, pages := 0, 0
	for {
		q := fmt.Sprintf("?limit=%d", limit)
		if cursor != "" {
			q += "&cursor=" + url.QueryEscape(cursor)
		}
		var page struct {
			Events     []json.RawMessage `json:"events"`
			NextCursor string            `json:"next_cursor"`
			HasMore    bool              `json:"has_more"`
		}
		st, raw, err := c.do("GET", "/api/v1/folders/"+url.PathEscape(folderID)+"/sync"+q, "", nil, nil, &page)
		if err != nil {
			return n, pages, err
		}
		if st != 200 {
			return n, pages, fmt.Errorf("sync: %d %s", st, raw)
		}
		pages++
		n += len(page.Events)
		cursor = page.NextCursor
		if !page.HasMore {
			return n, pages, nil
		}
	}
}

// ListFolders returns the number of folders the account sees.
func (c *Client) ListFolders() (int, error) {
	var out struct {
		Folders []json.RawMessage `json:"folders"`
	}
	st, raw, err := c.do("GET", "/api/v1/folders", "", nil, nil, &out)
	if err != nil {
		return 0, err
	}
	if st != 200 {
		return 0, fmt.Errorf("folders: %d %s", st, raw)
	}
	return len(out.Folders), nil
}

// DownloadSHA256 downloads an original and returns the SHA-256 of the body.
func (c *Client) DownloadSHA256(folderID, assetID string) (string, error) {
	req, err := http.NewRequest("GET", c.Base+"/api/v1/folders/"+url.PathEscape(folderID)+"/assets/"+url.PathEscape(assetID)+"/original", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("download: %s", resp.Status)
	}
	h := sha256.New()
	if _, err := io.Copy(h, resp.Body); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// RandomBytes returns n random bytes and their SHA-256.
func RandomBytes(n int) ([]byte, string) {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:])
}

// RSSBytes converts ru_maxrss to bytes (bytes on macOS, KiB on Linux).
func RSSBytes(ru *syscall.Rusage) int64 {
	if ru == nil {
		return 0
	}
	return int64(ru.Maxrss) * rssUnit
}
