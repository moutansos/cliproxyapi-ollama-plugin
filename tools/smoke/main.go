// Command smoke loads a built plugin library into a real CLIProxyAPI release
// binary for the current platform, points it at an in-process fake Ollama, and
// exercises discovery, listing, chat (OpenAI and Claude, streaming and not),
// managed context, and the management routes. It needs network access to
// download the CLIProxyAPI release; it never talks to a real Ollama.
package main

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/moutansos/cliproxyapi-ollama-plugin/internal/release"
)

const (
	clientKey = "smoke-client-key"
	mgmtKey   = "smoke-management-key"
	prefix    = "smoke"
)

func main() {
	pluginPath := flag.String("plugin", "", "path to the built plugin library")
	cpaVersion := flag.String("cpa-version", "8.0.11", "CLIProxyAPI release version to test against")
	cacheDir := flag.String("cache", filepath.Join(os.TempDir(), "cliproxyapi-ollama-smoke"), "download cache directory")
	expectVersion := flag.String("expect-version", "", "plugin version the host must report (optional)")
	timeout := flag.Duration("timeout", 3*time.Minute, "overall timeout")
	flag.Parse()
	if *pluginPath == "" {
		fatalf("-plugin is required")
	}
	deadline := time.Now().Add(*timeout)
	if err := run(*pluginPath, *cpaVersion, *cacheDir, *expectVersion, deadline); err != nil {
		fatalf("SMOKE FAILED (%s/%s): %v", runtime.GOOS, runtime.GOARCH, err)
	}
	fmt.Printf("SMOKE PASSED (%s/%s, CLIProxyAPI %s)\n", runtime.GOOS, runtime.GOARCH, *cpaVersion)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func step(format string, args ...any) { fmt.Printf("--- "+format+"\n", args...) }

func run(pluginPath, cpaVersion, cacheDir, expectVersion string, deadline time.Time) error {
	binary, err := fetchCPA(cpaVersion, cacheDir)
	if err != nil {
		return fmt.Errorf("fetch CLIProxyAPI: %w", err)
	}

	ollama := newFakeOllama()
	ollamaListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	go func() { _ = http.Serve(ollamaListener, ollama) }()
	defer ollamaListener.Close()

	work, err := os.MkdirTemp("", "cpa-smoke-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	platformDir := filepath.Join(work, "plugins", runtime.GOOS, runtime.GOARCH)
	if err := os.MkdirAll(platformDir, 0o755); err != nil {
		return err
	}
	lib, err := os.ReadFile(pluginPath)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(platformDir, release.LibraryName(release.PluginID, runtime.GOOS)), lib, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(work, "auths"), 0o700); err != nil {
		return err
	}
	port, err := freePort()
	if err != nil {
		return err
	}
	configPath := filepath.Join(work, "config.yaml")
	config := fmt.Sprintf(`config-version: 8
server:
  host: "127.0.0.1"
  port: %d
management:
  allow-remote: false
  secret-key: %q
  disable-control-panel: true
access:
  api-keys:
    - %q
oauth:
  auth-dir: %q
plugins:
  enabled: true
  dir: %q
  configs:
    cliproxyapi-ollama:
      enabled: true
      priority: 10
      base_url: "http://%s"
      model_prefix: %q
      refresh_interval_seconds: 10
`, port, mgmtKey, clientKey, filepath.ToSlash(filepath.Join(work, "auths")), filepath.ToSlash(filepath.Join(work, "plugins")), ollamaListener.Addr().String(), prefix)
	if err := os.WriteFile(configPath, []byte(config), 0o600); err != nil {
		return err
	}

	var logs safeBuffer
	cmd := exec.Command(binary, "-config", configPath)
	cmd.Dir = work
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	step("starting %s on port %d", filepath.Base(binary), port)
	if err := cmd.Start(); err != nil {
		return err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer func() {
		_ = cmd.Process.Kill()
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
		}
	}()

	c := &client{base: fmt.Sprintf("http://127.0.0.1:%d", port), exited: exited}
	errCheck := checks(c, ollama, expectVersion, deadline)
	if errCheck != nil {
		fmt.Fprintf(os.Stderr, "===== CLIProxyAPI log =====\n%s\n===== end log =====\n", logs.String())
		return errCheck
	}
	if strings.Contains(strings.ToLower(logs.String()), "panic") {
		fmt.Fprintf(os.Stderr, "===== CLIProxyAPI log =====\n%s\n", logs.String())
		return fmt.Errorf("CLIProxyAPI log contains a panic")
	}
	return nil
}

func checks(c *client, ollama *fakeOllama, expectVersion string, deadline time.Time) error {
	step("waiting for the plugin to publish %s/tiny:1b", prefix)
	var models modelList
	if err := waitFor(deadline, c, func() (bool, error) {
		status, body, err := c.do("GET", "/v1/models", clientKey, nil)
		if err != nil || status != http.StatusOK {
			return false, nil
		}
		models = modelList{}
		if err := json.Unmarshal(body, &models); err != nil {
			return false, nil
		}
		_, ok := models.find(prefix + "/tiny:1b")
		return ok, nil
	}); err != nil {
		return fmt.Errorf("model never appeared in /v1/models: %w", err)
	}

	step("plugin registration and CPAMC config fields")
	status, body, err := c.do("GET", "/v8/management/plugins", mgmtKey, nil)
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("list plugins: HTTP %d %v %s", status, err, body)
	}
	var plugins struct {
		Plugins []struct {
			ID               string            `json:"id"`
			Registered       bool              `json:"registered"`
			EffectiveEnabled bool              `json:"effective_enabled"`
			ConfigFields     []json.RawMessage `json:"config_fields"`
			Metadata         *struct {
				Version string `json:"version"`
			} `json:"metadata"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(body, &plugins); err != nil {
		return err
	}
	found := false
	for _, p := range plugins.Plugins {
		if p.ID != release.PluginID {
			continue
		}
		found = true
		if !p.Registered || !p.EffectiveEnabled {
			return fmt.Errorf("plugin not registered/enabled: %s", body)
		}
		if len(p.ConfigFields) < 10 {
			return fmt.Errorf("expected CPAMC config fields, got %d", len(p.ConfigFields))
		}
		if expectVersion != "" && (p.Metadata == nil || p.Metadata.Version != expectVersion) {
			return fmt.Errorf("plugin reports version %+v, want %s", p.Metadata, expectVersion)
		}
	}
	if !found {
		return fmt.Errorf("plugin %s not listed: %s", release.PluginID, body)
	}

	step("/v1/models metadata and embedding exclusion")
	tiny, _ := models.find(prefix + "/tiny:1b")
	if tiny.ContextLength != 4096 || tiny.DisplayName != prefix+"/tiny:1b" || tiny.OwnedBy != "ollama" {
		return fmt.Errorf("unexpected model entry %+v", tiny)
	}
	if _, ok := models.find(prefix + "/embed:latest"); ok {
		return fmt.Errorf("embedding model was advertised")
	}

	step("OpenAI chat completions (non-streaming)")
	chat := map[string]any{"model": prefix + "/tiny:1b", "messages": []any{map[string]any{"role": "user", "content": "ping"}}, "max_tokens": 16}
	status, body, err = c.do("POST", "/v1/chat/completions", clientKey, chat)
	if err != nil || status != http.StatusOK || !strings.Contains(string(body), `"pong"`) {
		return fmt.Errorf("chat: HTTP %d %v %s", status, err, body)
	}
	if opts := ollama.lastOptions(); opts["num_ctx"] != nil {
		return fmt.Errorf("observe mode sent num_ctx: %v", opts)
	}

	step("OpenAI chat completions (streaming)")
	chat["stream"] = true
	status, body, err = c.do("POST", "/v1/chat/completions", clientKey, chat)
	if err != nil || status != http.StatusOK || !strings.Contains(string(body), "data: [DONE]") || !strings.Contains(string(body), `"po"`) {
		return fmt.Errorf("stream: HTTP %d %v %s", status, err, body)
	}

	step("Claude Messages through host translation")
	claude := map[string]any{"model": prefix + "/tiny:1b", "max_tokens": 16, "messages": []any{map[string]any{"role": "user", "content": "ping"}}}
	status, body, err = c.do("POST", "/v1/messages", clientKey, claude)
	if err != nil || status != http.StatusOK || !strings.Contains(string(body), "pong") {
		return fmt.Errorf("claude messages: HTTP %d %v %s", status, err, body)
	}

	step("unknown model in the plugin namespace returns 404")
	missing := map[string]any{"model": prefix + "/missing:1b", "messages": []any{map[string]any{"role": "user", "content": "x"}}}
	status, body, _ = c.do("POST", "/v1/chat/completions", clientKey, missing)
	if status != http.StatusNotFound {
		return fmt.Errorf("missing model: HTTP %d %s", status, body)
	}

	step("managed override through the management API")
	override := map[string]any{"models": map[string]any{"tiny:1b": map[string]any{"policy": "managed", "num_ctx": 2048, "num_predict": 64, "keep_alive": "1m"}}}
	status, body, err = c.do("PATCH", "/v0/management/plugins/cliproxyapi-ollama/config", mgmtKey, override)
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("patch config: HTTP %d %v %s", status, err, body)
	}
	if err := waitFor(deadline, c, func() (bool, error) {
		_, body, err := c.do("GET", "/v1/models", clientKey, nil)
		if err != nil {
			return false, nil
		}
		var list modelList
		_ = json.Unmarshal(body, &list)
		m, _ := list.find(prefix + "/tiny:1b")
		return m.ContextLength == 2048 && m.MaxCompletionTokens == 64, nil
	}); err != nil {
		return fmt.Errorf("managed context never advertised: %w", err)
	}
	delete(chat, "stream")
	status, body, err = c.do("POST", "/v1/chat/completions", clientKey, chat)
	if err != nil || status != http.StatusOK {
		return fmt.Errorf("managed chat: HTTP %d %v %s", status, err, body)
	}
	opts := ollama.lastOptions()
	if fmt.Sprint(opts["num_ctx"]) != "2048" || fmt.Sprint(opts["num_predict"]) != "16" {
		return fmt.Errorf("managed request options = %v", opts)
	}

	step("diagnostics route (authenticated) reports a verified allocation")
	if status, _, _ := c.do("GET", "/v0/management/plugins/cliproxyapi-ollama/status", "", nil); status != http.StatusUnauthorized {
		return fmt.Errorf("status route without a key returned HTTP %d", status)
	}
	if err := waitFor(deadline, c, func() (bool, error) {
		status, body, err := c.do("GET", "/v0/management/plugins/cliproxyapi-ollama/status", mgmtKey, nil)
		if err != nil || status != http.StatusOK {
			return false, nil
		}
		return strings.Contains(string(body), `"verified": true`), nil
	}); err != nil {
		return fmt.Errorf("managed allocation not verified: %w", err)
	}
	return nil
}

type modelEntry struct {
	ID                  string `json:"id"`
	OwnedBy             string `json:"owned_by"`
	DisplayName         string `json:"display_name"`
	ContextLength       int    `json:"context_length"`
	MaxCompletionTokens int    `json:"max_completion_tokens"`
}

type modelList struct {
	Data []modelEntry `json:"data"`
}

func (l modelList) find(id string) (modelEntry, bool) {
	for _, m := range l.Data {
		if m.ID == id {
			return m, true
		}
	}
	return modelEntry{}, false
}

type client struct {
	base   string
	exited chan error
}

func (c *client) do(method, path, key string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		return 0, nil, err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if strings.HasPrefix(path, "/v1/messages") {
		req.Header.Set("anthropic-version", "2023-06-01")
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	return resp.StatusCode, data, err
}

func waitFor(deadline time.Time, c *client, cond func() (bool, error)) error {
	for time.Now().Before(deadline) {
		select {
		case err := <-c.exited:
			return fmt.Errorf("CLIProxyAPI exited: %v", err)
		default:
		}
		ok, err := cond()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("timed out")
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fetchCPA downloads and checksum-verifies the CLIProxyAPI release binary.
func fetchCPA(version, cacheDir string) (string, error) {
	arch := map[string]string{"amd64": "amd64", "arm64": "aarch64"}[runtime.GOARCH]
	if arch == "" {
		return "", fmt.Errorf("unsupported GOARCH %s", runtime.GOARCH)
	}
	ext := ".tar.gz"
	binaryName := "cli-proxy-api"
	if runtime.GOOS == "windows" {
		ext = ".zip"
		binaryName += ".exe"
	}
	asset := fmt.Sprintf("CLIProxyAPI_%s_%s_%s%s", version, runtime.GOOS, arch, ext)
	dir := filepath.Join(cacheDir, version, runtime.GOOS+"-"+arch)
	binaryPath := filepath.Join(dir, binaryName)
	if _, err := os.Stat(binaryPath); err == nil {
		return binaryPath, nil
	}
	base := "https://github.com/router-for-me/CLIProxyAPI/releases/download/v" + version + "/"
	sums, err := download(base + "checksums.txt")
	if err != nil {
		return "", err
	}
	archive, err := download(base + asset)
	if err != nil {
		return "", err
	}
	want := ""
	scanner := bufio.NewScanner(bytes.NewReader(sums))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == asset {
			want = fields[0]
		}
	}
	got := sha256.Sum256(archive)
	if want == "" || want != hex.EncodeToString(got[:]) {
		return "", fmt.Errorf("checksum mismatch for %s", asset)
	}
	step("downloaded and verified %s", asset)
	var exe []byte
	if ext == ".zip" {
		zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
		if err != nil {
			return "", err
		}
		for _, f := range zr.File {
			if filepath.Base(f.Name) == binaryName {
				rc, err := f.Open()
				if err != nil {
					return "", err
				}
				exe, err = io.ReadAll(rc)
				rc.Close()
				if err != nil {
					return "", err
				}
			}
		}
	} else {
		gz, err := gzip.NewReader(bytes.NewReader(archive))
		if err != nil {
			return "", err
		}
		tr := tar.NewReader(gz)
		for {
			h, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", err
			}
			if filepath.Base(h.Name) == binaryName {
				if exe, err = io.ReadAll(tr); err != nil {
					return "", err
				}
			}
		}
	}
	if len(exe) == 0 {
		return "", fmt.Errorf("%s not found in %s", binaryName, asset)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return binaryPath, os.WriteFile(binaryPath, exe, 0o755)
}

func download(url string) ([]byte, error) {
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}
