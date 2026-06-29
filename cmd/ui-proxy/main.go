/*
ui-proxy — Backend-for-Frontend for the agent-orc React UI.

Serves the pre-built React SPA (embedded at compile time from ./dist) and
reverse-proxies all /api/* requests to the operator's UI API server. Every
proxied request is authenticated using a Kubernetes projected ServiceAccount
token (audience: agentorc/ui) mounted into the pod at runtime.

Browser requests require no authentication — access control is enforced at
the network layer (NetworkPolicy + Ingress). OIDC/SAML can be added later as
an Ingress annotation or a separate proxy in front of this pod.

Build:

	cd ui && npm run build
	cp -r ui/dist cmd/ui-proxy/dist/
	go build ./cmd/ui-proxy

Note: go:embed ignores dotfiles, so cmd/ui-proxy/dist must contain at least one
non-hidden file (for example index.html) even before copying the SPA build.

Flags:

	--port           listening port (default 8080)
	--operator-addr  upstream operator UI API (default http://localhost:8083)
	--token-file     projected SA token path (default /var/run/secrets/agentorc/ui/token)
*/
package main

import (
	"embed"
	"flag"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"
)

//go:embed dist
var embeddedUI embed.FS

func main() {
	port := flag.String("port", "8080", "Listening port")
	operatorAddr := flag.String("operator-addr", "http://localhost:8083", "Upstream operator UI API address")
	tokenFile := flag.String("token-file", "/var/run/secrets/agentorc/ui/token", "Path to projected SA token")
	flag.Parse()

	upstream, err := url.Parse(*operatorAddr)
	if err != nil {
		slog.Error("invalid operator-addr", "err", err)
		os.Exit(1)
	}

	tm := newTokenManager(*tokenFile)
	go tm.refreshLoop()

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = upstream.Scheme
			req.URL.Host = upstream.Host
			req.Host = upstream.Host
			// Strip any Authorization the browser may have sent, then inject the SA token.
			req.Header.Del("Authorization")
			if tok := tm.get(); tok != "" {
				req.Header.Set("Authorization", "Bearer "+tok)
			}
		},
		// FlushInterval -1 means flush immediately — required for SSE streams.
		FlushInterval: -1,
	}

	uiFS, err := fs.Sub(embeddedUI, "dist")
	if err != nil {
		slog.Error("failed to sub embedded FS", "err", err)
		os.Exit(1)
	}
	fileServer := http.FileServer(http.FS(uiFS))

	mux := http.NewServeMux()

	// /api/* — proxy to the operator with SA token auth.
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		proxy.ServeHTTP(w, r)
	})

	// Everything else — serve the React SPA with index.html fallback for client-side routing.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		p := path.Clean(r.URL.Path)
		// If the path has a file extension and exists in the embedded FS, serve it directly.
		if p != "/" && strings.Contains(path.Base(p), ".") {
			fileServer.ServeHTTP(w, r)
			return
		}
		// For all other paths (client-side routes), serve index.html.
		r2 := r.Clone(r.Context())
		r2.URL.Path = "/"
		fileServer.ServeHTTP(w, r2)
	})

	addr := ":" + *port
	slog.Info("ui-proxy starting", "addr", addr, "upstream", *operatorAddr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("ui-proxy exited", "err", err)
		os.Exit(1)
	}
}

// tokenManager holds the current SA token and refreshes it from disk.
// Kubelet rotates the token roughly every 12 minutes; we re-read every 30s.
type tokenManager struct {
	path  string
	mu    sync.RWMutex
	value string
}

func newTokenManager(tokenPath string) *tokenManager {
	tm := &tokenManager{path: tokenPath}
	tm.reload() // load immediately so the first request has a token
	return tm
}

func (tm *tokenManager) get() string {
	tm.mu.RLock()
	defer tm.mu.RUnlock()
	return tm.value
}

func (tm *tokenManager) reload() {
	f, err := os.Open(tm.path)
	if err != nil {
		slog.Warn("could not read SA token", "path", tm.path, "err", err)
		return
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		slog.Warn("could not read SA token contents", "err", err)
		return
	}
	tok := strings.TrimSpace(string(b))
	tm.mu.Lock()
	tm.value = tok
	tm.mu.Unlock()
}

func (tm *tokenManager) refreshLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		tm.reload()
	}
}
