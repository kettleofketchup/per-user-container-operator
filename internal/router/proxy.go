package router

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"strings"
)

// applyHeaderPolicy is the exact header set reaching upstream, spec 217-219's
// trust boundary. It strips every trace of the caller's session — the
// caller-auth credential, the identity header, any Cookie, and Authorization
// unconditionally (whether or not it happens to be the configured
// callerAuth/identity header name) — and only THEN sets the upstream
// credential, so a router that set upstreamAuth before stripping could never
// delete its own just-set credential by accident. Connection, Upgrade and
// Sec-WebSocket-* are never touched here: they must survive for a 101 to
// complete, and this function has no code path that names them.
func applyHeaderPolicy(h http.Header, cfg Config) {
	h.Del(cfg.CallerAuthHeader)
	h.Del(cfg.IdentityHeader)
	h.Del("Cookie")
	h.Del("Authorization")

	if cfg.UpstreamAuthHeader == "" || len(cfg.UpstreamAuthSecret) == 0 {
		return
	}
	v := string(cfg.UpstreamAuthSecret)
	if cfg.UpstreamAuthScheme != "" {
		v = cfg.UpstreamAuthScheme + " " + v
	}
	h.Set(cfg.UpstreamAuthHeader, v)
}

// newReverseProxy returns an httputil.ReverseProxy targeting addr
// ("host:port"), applying the header policy on every forwarded request.
// net/http/httputil.ReverseProxy already implements RFC 7230's Connection
// upgrade handling (a hijack-and-copy full-duplex loop for a 101 response),
// so WebSocket upgrades need no bespoke plumbing here — only that this
// Director never deletes the headers that negotiate it. FlushInterval is set
// to flush immediately, so an SSE handler's first event reaches the client
// before the upstream response finishes, rather than sitting in a buffer.
func newReverseProxy(addr string, cfg Config) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		FlushInterval: -1,
		Director: func(req *http.Request) {
			applyHeaderPolicy(req.Header, cfg)
			req.URL.Scheme = "http"
			req.URL.Host = addr
		},
	}
}

// isUpgradeRequest reports whether r asks to upgrade the connection (e.g. a
// WebSocket handshake): the router tracks status.connections only for these,
// not for plain HTTP or SSE requests.
func isUpgradeRequest(r *http.Request) bool {
	return r.Header.Get("Upgrade") != "" && headerContainsToken(r.Header.Get("Connection"), "upgrade")
}

func headerContainsToken(v, token string) bool {
	for _, p := range strings.Split(v, ",") {
		if strings.EqualFold(strings.TrimSpace(p), token) {
			return true
		}
	}
	return false
}

// statusRecorder captures the status code an http.Handler wrote, for
// puc_router_requests_total{code}. It forwards Hijack and Flush explicitly:
// embedding http.ResponseWriter only promotes that interface's own three
// methods, so without these the type assertions httputil.ReverseProxy makes
// for the upgrade path (http.Hijacker) and for streaming (http.Flusher)
// would fail even though the underlying ResponseWriter satisfies both,
// silently downgrading every upgrade to a plain buffered response.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) code() int {
	if s.status == 0 {
		return http.StatusOK
	}
	return s.status
}

func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("underlying ResponseWriter does not support Hijack")
	}
	return hj.Hijack()
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
