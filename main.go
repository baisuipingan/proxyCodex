package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed static
var embeddedStatic embed.FS

type downloadTarget struct {
	Filename string
	Upstream string
}

type ccswitchRelease struct {
	Version string
	Items   map[string]downloadTarget
}

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type ccswitchResolver struct {
	client *http.Client
	logger *slog.Logger
	apiURL string
	ttl    time.Duration

	mu       sync.Mutex
	cached   *ccswitchRelease
	cachedAt time.Time
}

type ticketSigner struct {
	key []byte
	ttl time.Duration
}

func newTicketSigner() *ticketSigner {
	key := []byte(strings.TrimSpace(os.Getenv("DOWNLOAD_SIGN_KEY")))
	if len(key) == 0 {
		key = make([]byte, 32)
		_, _ = rand.Read(key)
	}
	return &ticketSigner{key: key, ttl: 15 * time.Minute}
}

func (s *ticketSigner) issue(platform string) string {
	expires := time.Now().Add(s.ttl).Unix()
	h := hmac.New(sha256.New, s.key)
	_, _ = fmt.Fprintf(h, "%s|%d", platform, expires)
	token := hex.EncodeToString(h.Sum(nil))
	return fmt.Sprintf("/download/%s?expires=%d&token=%s", platform, expires, token)
}

func (s *ticketSigner) verify(platform string, expires int64, token string) bool {
	if time.Now().Unix() > expires {
		return false
	}
	h := hmac.New(sha256.New, s.key)
	_, _ = fmt.Fprintf(h, "%s|%d", platform, expires)
	expected := hex.EncodeToString(h.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(token))
}

func loadTargets() map[string]downloadTarget {
	return map[string]downloadTarget{
		"mac-arm64": {
			Filename: "Codex-mac-arm64.dmg",
			Upstream: envOr("CODEX_MAC_ARM64_URL", "https://persistent.oaistatic.com/codex-app-prod/Codex.dmg"),
		},
		"mac-x64": {
			Filename: "Codex-mac-x64.dmg",
			Upstream: envOr("CODEX_MAC_X64_URL", "https://persistent.oaistatic.com/codex-app-prod/Codex-latest-x64.dmg"),
		},
		"win-x64": {
			Filename: "Codex-Windows-x64.msix",
			Upstream: envOr("CODEX_WIN_X64_URL", "https://codexapp.agentsmirror.com/latest/win-x64"),
		},
		"win-arm64": {
			Filename: "Codex-Windows-arm64.msix",
			Upstream: envOr("CODEX_WIN_ARM64_URL", "https://codexapp.agentsmirror.com/latest/win-arm64"),
		},
	}
}

func main() {
	addr := envOr("ADDR", ":8080")
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	client := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   15 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:          32,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		},
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           newMux(client, logger, loadTargets()),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		logger.Info("codex download proxy listening", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server stopped unexpectedly", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
	}
}

func newMux(client *http.Client, logger *slog.Logger, targets map[string]downloadTarget) http.Handler {
	return newMuxWithResolver(client, logger, targets, newCCSwitchResolver(client, logger), newTicketSigner())
}

func newMuxWithResolver(client *http.Client, logger *slog.Logger, targets map[string]downloadTarget, resolver *ccswitchResolver, signer *ticketSigner) http.Handler {
	static, err := fs.Sub(embeddedStatic, "static")
	if err != nil {
		panic(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(static)))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/api/ccswitch", ccswitchAPIHandler(resolver, logger))
	mux.HandleFunc("/api/ticket", ticketHandler(targets, resolver, signer))
	mux.HandleFunc("/download/{platform}", downloadHandler(client, logger, targets, resolver, signer))
	return mux
}

func downloadHandler(client *http.Client, logger *slog.Logger, targets map[string]downloadTarget, resolver *ccswitchResolver, signer *ticketSigner) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		platform := r.PathValue("platform")
		_, staticOK := targets[platform]
		dynamicOK := resolver != nil && strings.HasPrefix(platform, "ccswitch-")
		if !staticOK && !dynamicOK {
			http.NotFound(w, r)
			return
		}

		expires, err := strconv.ParseInt(r.URL.Query().Get("expires"), 10, 64)
		token := r.URL.Query().Get("token")
		if err != nil || !signer.verify(platform, expires, token) {
			http.Error(w, "download link invalid or expired", http.StatusForbidden)
			return
		}

		if target, ok := targets[platform]; ok {
			streamDownload(w, r, target, client, logger)
			return
		}

		release, err := resolver.resolve(r.Context())
		if err != nil {
			logger.Error("resolve cc-switch release failed", "error", err)
			http.Error(w, "cc-switch release unavailable", http.StatusBadGateway)
			return
		}
		target, ok := release.Items[platform]
		if !ok {
			http.NotFound(w, r)
			return
		}
		streamDownload(w, r, target, client, logger)
	}
}

func ticketHandler(targets map[string]downloadTarget, resolver *ccswitchResolver, signer *ticketSigner) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		platform := r.URL.Query().Get("platform")
		_, staticOK := targets[platform]
		dynamicOK := resolver != nil && strings.HasPrefix(platform, "ccswitch-")
		if !staticOK && !dynamicOK {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_ = json.NewEncoder(w).Encode(map[string]string{"url": signer.issue(platform)})
	}
}

func streamDownload(w http.ResponseWriter, r *http.Request, target downloadTarget, client *http.Client, logger *slog.Logger) {
	upstreamReq, err := http.NewRequestWithContext(r.Context(), r.Method, target.Upstream, nil)
	if err != nil {
		logger.Error("create upstream request failed", "upstream", target.Upstream, "error", err)
		http.Error(w, "download unavailable", http.StatusInternalServerError)
		return
	}
	upstreamReq.Header.Set("User-Agent", "CodexDownloadProxy/1.0")
	if r.Header.Get("Range") != "" {
		upstreamReq.Header.Set("Range", r.Header.Get("Range"))
	}

	resp, err := client.Do(upstreamReq)
	if err != nil {
		logger.Error("upstream request failed", "upstream", target.Upstream, "error", err)
		http.Error(w, "download unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyHeaders(w.Header(), resp.Header,
		"Content-Type",
		"Content-Disposition",
		"Content-Length",
		"Content-Range",
		"Accept-Ranges",
		"ETag",
		"Last-Modified",
	)
	w.Header().Set("Content-Disposition", `attachment; filename="`+target.Filename+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)

	if r.Method == http.MethodHead {
		return
	}

	if _, err := io.Copy(w, resp.Body); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("stream upstream body failed", "upstream", target.Upstream, "error", err)
	}
}

func copyHeaders(dst, src http.Header, names ...string) {
	for _, name := range names {
		for _, value := range src.Values(name) {
			dst.Add(name, value)
		}
	}
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func newCCSwitchResolver(client *http.Client, logger *slog.Logger) *ccswitchResolver {
	return &ccswitchResolver{
		client: client,
		logger: logger,
		apiURL: envOr("CCSWITCH_API_URL", "https://api.github.com/repos/farion1231/cc-switch/releases/latest"),
		ttl:    15 * time.Minute,
	}
}

func (r *ccswitchResolver) resolve(ctx context.Context) (*ccswitchRelease, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.cached != nil && time.Since(r.cachedAt) < r.ttl {
		return r.cached, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "CodexDownloadProxy/1.0")

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github release api returned %s", resp.Status)
	}

	var payload struct {
		TagName string         `json:"tag_name"`
		Assets  []releaseAsset `json:"assets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}

	release, err := buildCCSwitchRelease(payload.TagName, payload.Assets)
	if err != nil {
		return nil, err
	}

	r.cached = release
	r.cachedAt = time.Now()
	return release, nil
}

func buildCCSwitchRelease(version string, assets []releaseAsset) (*ccswitchRelease, error) {
	specs := []struct {
		id    string
		match func(string) bool
	}{
		{"ccswitch-mac", func(name string) bool {
			return strings.HasSuffix(name, ".dmg") && strings.Contains(name, "macOS")
		}},
		{"ccswitch-win-x64", func(name string) bool {
			return strings.HasSuffix(name, ".msi") && strings.Contains(name, "Windows") && !strings.Contains(strings.ToLower(name), "arm64")
		}},
		{"ccswitch-win-arm64", func(name string) bool {
			return strings.HasSuffix(name, ".msi") && strings.Contains(name, "Windows-arm64")
		}},
		{"ccswitch-linux-x64", func(name string) bool {
			return strings.HasSuffix(name, ".deb") && strings.Contains(name, "Linux-x86_64")
		}},
		{"ccswitch-linux-arm64", func(name string) bool {
			return strings.HasSuffix(name, ".deb") && strings.Contains(name, "Linux-arm64")
		}},
	}

	items := make(map[string]downloadTarget, len(specs))
	for _, spec := range specs {
		var found *releaseAsset
		for i := range assets {
			if spec.match(assets[i].Name) {
				found = &assets[i]
				break
			}
		}
		if found == nil {
			return nil, fmt.Errorf("cc-switch %s asset not found", spec.id)
		}
		items[spec.id] = downloadTarget{
			Filename: found.Name,
			Upstream: found.BrowserDownloadURL,
		}
	}

	return &ccswitchRelease{Version: version, Items: items}, nil
}

func ccswitchAPIHandler(resolver *ccswitchResolver, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		release, err := resolver.resolve(r.Context())
		if err != nil {
			logger.Error("resolve cc-switch release failed", "error", err)
			http.Error(w, "cc-switch release unavailable", http.StatusBadGateway)
			return
		}

		order := []string{
			"ccswitch-mac",
			"ccswitch-win-x64",
			"ccswitch-win-arm64",
			"ccswitch-linux-x64",
			"ccswitch-linux-arm64",
		}
		items := make([]map[string]string, 0, len(order))
		for _, id := range order {
			target, ok := release.Items[id]
			if !ok {
				continue
			}
			items = append(items, map[string]string{
				"id":     id,
				"name":   target.Filename,
				"detail": ccswitchDetail(id),
			})
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"version": release.Version,
			"items":   items,
		}); err != nil {
			logger.Error("encode cc-switch api response failed", "error", err)
		}
	}
}

func ccswitchDetail(id string) string {
	switch id {
	case "ccswitch-mac":
		return "macOS 最新版 DMG，本站代理下载"
	case "ccswitch-win-x64":
		return "Windows x64 最新版 MSI，本站代理下载"
	case "ccswitch-win-arm64":
		return "Windows ARM64 最新版 MSI，本站代理下载"
	case "ccswitch-linux-x64":
		return "Linux x86_64 最新版 DEB，本站代理下载"
	case "ccswitch-linux-arm64":
		return "Linux ARM64 最新版 DEB，本站代理下载"
	default:
		return "最新版，本站代理下载"
	}
}
