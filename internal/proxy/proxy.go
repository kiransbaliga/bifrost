package proxy

import (
	"compress/gzip"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/valkyrie-stack/bifrost/internal/domain"
)

// bufferPool implements httputil.BufferPool using sync.Pool with 32KB buffers (ADR-003).
type bufferPool struct {
	pool sync.Pool
}

func newBufferPool() *bufferPool {
	return &bufferPool{
		pool: sync.Pool{
			New: func() any {
				b := make([]byte, 32*1024) // 32 KB buffer
				return &b
			},
		},
	}
}

func (bp *bufferPool) Get() []byte {
	return *(bp.pool.Get().(*[]byte))
}

func (bp *bufferPool) Put(b []byte) {
	if cap(b) >= 32*1024 {
		b = b[:32*1024]
		bp.pool.Put(&b)
	}
}

type gzipResponseWriter struct {
	io.Writer
	http.ResponseWriter
}

func (w gzipResponseWriter) Write(b []byte) (int, error) {
	return w.Writer.Write(b)
}

// Proxy is the in-process reverse proxy and static file server.
type Proxy struct {
	routes       atomic.Pointer[map[string]*domain.RouteTarget]
	bufPool      *bufferPool
	certManager  *autocert.Manager
	tlsConfig    *tls.Config
	reverseProxy *httputil.ReverseProxy
	dashboardMux http.Handler
	hostDomain   string
	enableTLS    bool
}

// NewProxy constructs an in-process proxy with zero-allocation routing and buffer pooling.
func NewProxy(dataDir, hostDomain, acmeEmail string, enableTLS bool, dashboardMux http.Handler) *Proxy {
	p := &Proxy{
		bufPool:      newBufferPool(),
		dashboardMux: dashboardMux,
		hostDomain:   hostDomain,
		enableTLS:    enableTLS,
	}

	initialRoutes := make(map[string]*domain.RouteTarget)
	p.routes.Store(&initialRoutes)

	// Configure Let's Encrypt TLS if enabled
	if enableTLS {
		certsDir := filepath.Join(dataDir, "certs")
		p.certManager = &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			Cache:      autocert.DirCache(certsDir),
			Email:      acmeEmail,
			HostPolicy: p.hostPolicy,
		}
		p.tlsConfig = &tls.Config{
			GetCertificate: p.certManager.GetCertificate,
			NextProtos:     []string{"h2", "http/1.1", acme.ALPNProto},
			MinVersion:     tls.VersionTLS12,
		}
	}

	// Setup underlying reverse proxy
	p.reverseProxy = &httputil.ReverseProxy{
		BufferPool:    p.bufPool,
		FlushInterval: -1, // Immediate flush for WebSockets, SSE, and streaming
		Director:      func(req *http.Request) {}, // Custom routing handled in ServeHTTP
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprintf(w, `{"error":"Bad Gateway","message":"Target upstream unreachable: %s"}`, err.Error())
		},
	}

	return p
}

// UpdateRoutes atomically replaces the active routing table (ADR-003: lock-free update).
func (p *Proxy) UpdateRoutes(routes []*domain.RouteTarget) {
	newMap := make(map[string]*domain.RouteTarget, len(routes))
	for _, r := range routes {
		newMap[strings.ToLower(r.Domain)] = r
	}
	p.routes.Store(&newMap)
}

func (p *Proxy) hostPolicy(_ context.Context, host string) error {
	host = strings.ToLower(host)
	if p.hostDomain != "" && host == strings.ToLower(p.hostDomain) {
		return nil
	}
	routes := p.routes.Load()
	if routes != nil {
		if _, ok := (*routes)[host]; ok {
			return nil
		}
	}
	return fmt.Errorf("domain %q not recognized or authorized for TLS", host)
}

// ServeHTTP inspects incoming domain and dispatches to Dashboard, Static Engine, or Upstream.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(r.Host)
	if colon := strings.IndexByte(host, ':'); colon != -1 {
		host = host[:colon]
	}

	// Check if request is for the Bifrost control plane dashboard/API
	if p.hostDomain != "" && host == strings.ToLower(p.hostDomain) {
		p.dashboardMux.ServeHTTP(w, r)
		return
	}

	// Look up route in lock-free routing table
	routeMap := p.routes.Load()
	var target *domain.RouteTarget
	if routeMap != nil {
		target = (*routeMap)[host]
	}

	if target == nil {
		// Fallback: If no route matches and accessing via direct IP or fallback, show dashboard
		if p.hostDomain == "" || host == "localhost" || host == "127.0.0.1" {
			p.dashboardMux.ServeHTTP(w, r)
			return
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, "<html><body style='font-family:sans-serif;text-align:center;padding:50px;'><h2>404 - Domain Unmapped</h2><p>No active project mapped to <b>%s</b> on Bifrost.</p></body></html>", host)
		return
	}

	// ADR-006: Static Direct Serve Mode (Zero-Container Hosting)
	if target.IsStatic {
		p.serveStatic(w, r, target.StaticPath)
		return
	}

	// Dynamic Container Reverse Proxy
	p.proxyToUpstream(w, r, target.UpstreamURL)
}

func (p *Proxy) serveStatic(w http.ResponseWriter, r *http.Request, rootDir string) {
	reqPath := filepath.Clean(r.URL.Path)
	fullPath := filepath.Join(rootDir, reqPath)

	fi, err := os.Stat(fullPath)
	// SPA Fallback: If file does not exist, serve root index.html
	if err != nil || fi.IsDir() {
		indexPath := filepath.Join(rootDir, "index.html")
		if fi != nil && fi.IsDir() {
			dirIndex := filepath.Join(fullPath, "index.html")
			if _, err := os.Stat(dirIndex); err == nil {
				fullPath = dirIndex
			} else {
				fullPath = indexPath
			}
		} else {
			fullPath = indexPath
		}
	}

	// Set caching headers
	if strings.Contains(r.URL.Path, "/assets/") || strings.HasSuffix(r.URL.Path, ".js") || strings.HasSuffix(r.URL.Path, ".css") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}

	// Gzip compression support
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Del("Content-Length")
		gz := gzip.NewWriter(w)
		defer gz.Close()

		f, err := os.Open(fullPath)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()

		// Stream directly with gzip wrapper
		http.ServeContent(gzipResponseWriter{Writer: gz, ResponseWriter: w}, r, filepath.Base(fullPath), time.Time{}, f)
		return
	}

	http.ServeFile(w, r, fullPath)
}

func (p *Proxy) proxyToUpstream(w http.ResponseWriter, r *http.Request, upstreamURL string) {
	target, err := url.Parse(upstreamURL)
	if err != nil {
		http.Error(w, "Invalid upstream URL", http.StatusInternalServerError)
		return
	}

	// Adjust headers for upstream
	r.URL.Scheme = target.Scheme
	r.URL.Host = target.Host
	r.Header.Set("X-Forwarded-Host", r.Host)
	r.Header.Set("X-Forwarded-Proto", "http")
	if r.TLS != nil {
		r.Header.Set("X-Forwarded-Proto", "https")
	}

	p.reverseProxy.ServeHTTP(w, r)
}

// StartServers initializes HTTP (:80) and optionally HTTPS (:443).
func (p *Proxy) StartServers(httpPort, httpsPort int) error {
	httpAddr := fmt.Sprintf(":%d", httpPort)
	httpsAddr := fmt.Sprintf(":%d", httpsPort)

	if !p.enableTLS || p.certManager == nil {
		log.Printf("[Proxy] Starting in HTTP-only mode on %s", httpAddr)
		return http.ListenAndServe(httpAddr, p)
	}

	// If TLS is enabled:
	// 1. Port 80 serves ACME HTTP-01 challenges and redirects other traffic to HTTPS.
	// 2. Port 443 serves TLS with autocert.
	log.Printf("[Proxy] Starting HTTPS with Let's Encrypt on %s and redirector on %s", httpsAddr, httpAddr)

	go func() {
		httpServer := &http.Server{
			Addr:         httpAddr,
			Handler:      p.certManager.HTTPHandler(http.HandlerFunc(p.redirectToHTTPS)),
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 10 * time.Second,
		}
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[Proxy] HTTP-01 redirector exited: %v", err)
		}
	}()

	tlsServer := &http.Server{
		Addr:         httpsAddr,
		Handler:      p,
		TLSConfig:    p.tlsConfig,
		ReadTimeout:  60 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	return tlsServer.ListenAndServeTLS("", "")
}

func (p *Proxy) redirectToHTTPS(w http.ResponseWriter, r *http.Request) {
	target := "https://" + r.Host + r.URL.RequestURI()
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}

// CheckPortAvailable verifies whether a TCP port is free to bind.
func CheckPortAvailable(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}
