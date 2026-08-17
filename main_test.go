package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func ticketURL(t *testing.T, mux http.Handler, platform string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/ticket?platform="+platform, nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("ticket status = %d, want %d", rr.Code, http.StatusOK)
	}
	var resp struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode ticket response: %v", err)
	}
	if resp.URL == "" {
		t.Fatalf("ticket response has empty url")
	}
	return resp.URL
}

func TestIndexPage(t *testing.T) {
	mux := newMux(http.DefaultClient, discardLogger(), map[string]downloadTarget{})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if !strings.Contains(rr.Body.String(), "Codex下载") {
		t.Fatalf("index page does not contain expected heading")
	}
}

func TestUnknownPlatform(t *testing.T) {
	mux := newMux(http.DefaultClient, discardLogger(), map[string]downloadTarget{})

	req := httptest.NewRequest(http.MethodGet, "/download/not-a-platform", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusNotFound)
	}
}

func TestConfiguredWindowsProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.ms-appx")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("msix"))
	}))
	defer upstream.Close()

	targets := map[string]downloadTarget{
		"win-x64": {
			Filename: "Codex-Windows-x64.msix",
			Upstream: upstream.URL,
		},
	}
	mux := newMux(http.DefaultClient, discardLogger(), targets)

	req := httptest.NewRequest(http.MethodGet, ticketURL(t, mux, "win-x64"), nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if rr.Body.String() != "msix" {
		t.Fatalf("body = %q, want %q", rr.Body.String(), "msix")
	}
	if got := rr.Header().Get("Content-Disposition"); !strings.Contains(got, "Codex-Windows-x64.msix") {
		t.Fatalf("content-disposition = %q, want filename Codex-Windows-x64.msix", got)
	}
}

func TestDownloadProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") == "" {
			t.Error("upstream request did not include User-Agent")
		}
		if got := r.Header.Get("Range"); got != "bytes=0-99" {
			t.Errorf("range = %q, want %q", got, "bytes=0-99")
		}
		w.Header().Set("Content-Type", "application/x-apple-diskimage")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte("hello"))
	}))
	defer upstream.Close()

	targets := map[string]downloadTarget{
		"mac-arm64": {
			Filename: "Codex-mac-arm64.dmg",
			Upstream: upstream.URL,
		},
	}
	mux := newMux(http.DefaultClient, discardLogger(), targets)

	req := httptest.NewRequest(http.MethodGet, ticketURL(t, mux, "mac-arm64"), nil)
	req.Header.Set("Range", "bytes=0-99")
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusPartialContent)
	}
	if rr.Body.String() != "hello" {
		t.Fatalf("body = %q, want %q", rr.Body.String(), "hello")
	}
	if got := rr.Header().Get("Content-Disposition"); !strings.Contains(got, "Codex-mac-arm64.dmg") {
		t.Fatalf("content-disposition = %q, want filename Codex-mac-arm64.dmg", got)
	}
}

func TestCCSwitchAPIAndDownload(t *testing.T) {
	assetServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("cc-switch"))
	}))
	defer assetServer.Close()

	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "v3.19.2",
			"assets": []map[string]string{
				{"name": "CC-Switch-v3.19.2-macOS.dmg", "browser_download_url": assetServer.URL + "/mac.dmg"},
				{"name": "CC-Switch-v3.19.2-Windows.msi", "browser_download_url": assetServer.URL + "/win.msi"},
				{"name": "CC-Switch-v3.19.2-Windows-arm64.msi", "browser_download_url": assetServer.URL + "/win-arm.msi"},
				{"name": "CC-Switch-v3.19.2-Linux-x86_64.deb", "browser_download_url": assetServer.URL + "/linux.deb"},
				{"name": "CC-Switch-v3.19.2-Linux-arm64.deb", "browser_download_url": assetServer.URL + "/linux-arm.deb"},
			},
		})
	}))
	defer github.Close()

	resolver := &ccswitchResolver{
		client: http.DefaultClient,
		logger: discardLogger(),
		apiURL: github.URL,
		ttl:    time.Hour,
	}
	mux := newMuxWithResolver(http.DefaultClient, discardLogger(), map[string]downloadTarget{}, resolver, newTicketSigner(), nil)

	apiReq := httptest.NewRequest(http.MethodGet, "/api/ccswitch", nil)
	apiRR := httptest.NewRecorder()
	mux.ServeHTTP(apiRR, apiReq)

	if apiRR.Code != http.StatusOK {
		t.Fatalf("api status = %d, want %d", apiRR.Code, http.StatusOK)
	}
	if !strings.Contains(apiRR.Body.String(), "v3.19.2") || !strings.Contains(apiRR.Body.String(), "ccswitch-mac") {
		t.Fatalf("api response missing version or items: %s", apiRR.Body.String())
	}

	dlReq := httptest.NewRequest(http.MethodGet, ticketURL(t, mux, "ccswitch-mac"), nil)
	dlRR := httptest.NewRecorder()
	mux.ServeHTTP(dlRR, dlReq)

	if dlRR.Code != http.StatusOK {
		t.Fatalf("download status = %d, want %d", dlRR.Code, http.StatusOK)
	}
	if dlRR.Body.String() != "cc-switch" {
		t.Fatalf("download body = %q, want %q", dlRR.Body.String(), "cc-switch")
	}
	if got := dlRR.Header().Get("Content-Disposition"); !strings.Contains(got, "CC-Switch-v3.19.2-macOS.dmg") {
		t.Fatalf("content-disposition = %q, want filename CC-Switch-v3.19.2-macOS.dmg", got)
	}
}

func TestDownloadLinkRejectedWithoutTicket(t *testing.T) {
	mux := newMux(http.DefaultClient, discardLogger(), map[string]downloadTarget{
		"mac-arm64": {
			Filename: "Codex-mac-arm64.dmg",
			Upstream: "http://127.0.0.1:1/unused",
		},
	})

	req := httptest.NewRequest(http.MethodGet, "/download/mac-arm64", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusForbidden)
	}
}

func TestTicketVerification(t *testing.T) {
	signer := &ticketSigner{key: []byte("test-key"), ttl: time.Minute}

	rawURL := signer.issue("mac-arm64")
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse signed url: %v", err)
	}
	expires, err := strconv.ParseInt(parsed.Query().Get("expires"), 10, 64)
	if err != nil {
		t.Fatalf("parse expires: %v", err)
	}
	token := parsed.Query().Get("token")

	if !signer.verify("mac-arm64", expires, token) {
		t.Error("valid ticket was rejected")
	}
	if signer.verify("mac-arm64", expires-3600, token) {
		t.Error("expired ticket was accepted")
	}
	if signer.verify("win-x64", expires, token) {
		t.Error("ticket was accepted for a different platform")
	}
	if signer.verify("mac-arm64", expires, "bad-token") {
		t.Error("tampered token was accepted")
	}
}

func TestMirrorServesLocalFile(t *testing.T) {
	dir := t.TempDir()
	filename := "Codex-mac-arm64.dmg"
	if err := os.WriteFile(filepath.Join(dir, filename), []byte("local"), 0o644); err != nil {
		t.Fatal(err)
	}

	targets := map[string]downloadTarget{
		"mac-arm64": {Filename: filename, Upstream: "http://127.0.0.1:1/unused"},
	}
	m := newMirror(dir, http.DefaultClient, discardLogger(), nil, targets)
	m.mu.Lock()
	m.items["mac-arm64"] = mirrorItem{Filename: filename, Size: 5, Updated: time.Now()}
	m.mu.Unlock()

	mux := newMuxWithResolver(http.DefaultClient, discardLogger(), targets, nil, newTicketSigner(), m)

	req := httptest.NewRequest(http.MethodGet, ticketURL(t, mux, "mac-arm64"), nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if rr.Body.String() != "local" {
		t.Fatalf("body = %q, want %q", rr.Body.String(), "local")
	}
	if got := rr.Header().Get("Content-Disposition"); !strings.Contains(got, filename) {
		t.Fatalf("content-disposition = %q, want filename %s", got, filename)
	}
}
