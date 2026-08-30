package router

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// dialAndUpgrade sends a real WebSocket-style upgrade request through addr
// (a proxy or a backend listening on real TCP) and returns the raw
// connection plus the parsed response, so the caller can drive or verify a
// genuine 101 handshake end to end.
func dialAndUpgrade(t *testing.T, addr string, extraHeaders map[string]string) (net.Conn, *http.Response, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	req, err := http.NewRequest(http.MethodGet, "http://"+addr+"/", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Sec-WebSocket-Version", "13")
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	if err := req.Write(conn); err != nil {
		t.Fatalf("write request: %v", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return conn, resp, br
}

// hijackAndRespond101 hijacks w and writes a bare 101 Switching Protocols
// response, for use inside a backend handler under test.
func hijackAndRespond101(w http.ResponseWriter) (net.Conn, error) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, nil
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	if _, err := buf.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n"); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := buf.Flush(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

// TestUpstreamHeaderSet is the exact header set reaching upstream, asserted
// by VALUE, not presence: the real consumer configuration uses
// "Authorization" for both callerAuth.header and workspace.upstreamAuth.header,
// so the two must never collide — the strip must run before the set, or a
// router that sets upstreamAuth first would delete its own just-set
// credential.
func TestUpstreamHeaderSet(t *testing.T) {
	t.Run("case A upstreamAuth set sharing the caller's header name", func(t *testing.T) {
		capturedCh := make(chan http.Header, 1)
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedCh <- r.Header.Clone()
			conn, err := hijackAndRespond101(w)
			if err != nil || conn == nil {
				return
			}
			defer func() { _ = conn.Close() }()
		}))
		defer backend.Close()

		cfg := Config{
			CallerAuthHeader:   "Authorization",
			IdentityHeader:     "X-User-Id",
			UpstreamAuthHeader: "Authorization",
			UpstreamAuthScheme: "Bearer",
			UpstreamAuthSecret: []byte("upstream-secret"),
		}
		proxy := httptest.NewServer(newReverseProxy(backend.Listener.Addr().String(), cfg))
		defer proxy.Close()
		proxyAddr := strings.TrimPrefix(proxy.URL, "http://")

		conn, resp, _ := dialAndUpgrade(t, proxyAddr, map[string]string{
			"Authorization": "Bearer caller-token",
			"X-User-Id":     "alice",
			"Cookie":        "session=abc",
		})
		defer func() { _ = conn.Close() }()
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusSwitchingProtocols {
			t.Fatalf("status = %d, want 101 (the upgrade must complete)", resp.StatusCode)
		}

		var captured http.Header
		select {
		case captured = <-capturedCh:
		case <-time.After(2 * time.Second):
			t.Fatal("backend never received the request")
		}

		if got := captured.Values("Authorization"); len(got) != 1 || got[0] != "Bearer upstream-secret" {
			t.Fatalf("upstream Authorization = %v, want exactly one value 'Bearer upstream-secret'", got)
		}
		if v := captured.Get("X-User-Id"); v != "" {
			t.Fatalf("identity header must be stripped, got %q", v)
		}
		if v := captured.Get("Cookie"); v != "" {
			t.Fatalf("Cookie must be stripped, got %q", v)
		}
		if captured.Get("Connection") == "" || captured.Get("Upgrade") == "" {
			t.Fatalf("Connection/Upgrade must survive so the 101 completes: Connection=%q Upgrade=%q", captured.Get("Connection"), captured.Get("Upgrade"))
		}
	})

	t.Run("case B upstreamAuth unset", func(t *testing.T) {
		capturedCh := make(chan http.Header, 1)
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			capturedCh <- r.Header.Clone()
			w.WriteHeader(http.StatusOK)
		}))
		defer backend.Close()

		cfg := Config{CallerAuthHeader: "Authorization", IdentityHeader: "X-User-Id"}
		proxy := httptest.NewServer(newReverseProxy(backend.Listener.Addr().String(), cfg))
		defer proxy.Close()

		req, err := http.NewRequest(http.MethodGet, proxy.URL+"/", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("Authorization", "Bearer caller-token")
		req.Header.Set("X-User-Id", "alice")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		var captured http.Header
		select {
		case captured = <-capturedCh:
		case <-time.After(2 * time.Second):
			t.Fatal("backend never received the request")
		}

		if got := captured.Values("Authorization"); len(got) != 0 {
			t.Fatalf("Authorization must be absent entirely when upstreamAuth is unset, got %v", got)
		}
	})
}

// TestWebSocketUpgradeCompletes drives a real 101 upgrade through the proxy
// and round-trips bytes both directions over the resulting raw connection.
func TestWebSocketUpgradeCompletes(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, err := hijackAndRespond101(w)
		if err != nil || conn == nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.Copy(conn, conn) // echo whatever the client sends
	}))
	defer backend.Close()

	proxy := httptest.NewServer(newReverseProxy(backend.Listener.Addr().String(), Config{}))
	defer proxy.Close()
	proxyAddr := strings.TrimPrefix(proxy.URL, "http://")

	conn, resp, br := dialAndUpgrade(t, proxyAddr, nil)
	defer func() { _ = conn.Close() }()
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}

	payload := []byte("hello-upgrade-roundtrip")
	if _, err := conn.Write(payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	echoed := make([]byte, len(payload))
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	if _, err := io.ReadFull(br, echoed); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(echoed) != string(payload) {
		t.Fatalf("echo = %q, want %q", echoed, payload)
	}
}

// TestSSEStreamsIncrementally asserts the first event is observed by the
// client BEFORE the handler returns: a buffering proxy passes any
// end-to-end body-equality assertion but fails exactly this one.
func TestSSEStreamsIncrementally(t *testing.T) {
	handlerDone := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Error("backend ResponseWriter does not support Flush")
			return
		}
		_, _ = w.Write([]byte("data: first\n\n"))
		fl.Flush()
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte("data: second\n\n"))
		fl.Flush()
		close(handlerDone)
	}))
	defer backend.Close()

	proxy := httptest.NewServer(newReverseProxy(backend.Listener.Addr().String(), Config{}))
	defer proxy.Close()

	resp, err := http.Get(proxy.URL)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	br := bufio.NewReader(resp.Body)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read first event: %v", err)
	}
	if !strings.Contains(line, "first") {
		t.Fatalf("first line = %q, want to contain 'first'", line)
	}

	select {
	case <-handlerDone:
		t.Fatal("the backend handler had already finished by the time the first SSE event reached the client -- the proxy buffered instead of streaming incrementally")
	default:
	}
}
