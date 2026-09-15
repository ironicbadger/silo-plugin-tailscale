package tailscale

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProxyPreservesPlaybackAndReplacesSpoofedHeaders(t *testing.T) {
	got := make(chan *http.Request, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Clone(context.Background())
		w.Header().Set("Content-Range", "bytes 2-4/6")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "cde")
	}))
	defer upstream.Close()
	proxy, transport := NewProxy(strings.TrimPrefix(upstream.URL, "http://"), "host-token")
	defer transport.CloseIdleConnections()
	front := httptest.NewServer(proxy)
	defer front.Close()
	req, _ := http.NewRequest("GET", front.URL+"/media/a%2Fb?token=opaque%2Bvalue&x=1&x=2", nil)
	req.Host = "silo.example.test:8096"
	req.Header.Set("Range", "bytes=2-4")
	req.Header.Set("Authorization", "Bearer playback-token")
	req.Header["X-Silo-Ingress-Token"] = []string{"spoof-one", "spoof-two"}
	req.Header.Set("Connection", "X-Silo-Ingress-Token, X-Forwarded-Proto")
	req.Header.Set("Forwarded", "for=spoof;proto=http")
	req.Header.Set("X-Forwarded-Proto", "http")
	req.Header.Set("X-Forwarded-For", "spoof")
	req.Header.Set("X-Forwarded-Host", "evil.invalid")
	req.Header.Set("X-Real-IP", "spoof")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 206 || string(b) != "cde" || resp.Header.Get("Content-Range") != "bytes 2-4/6" {
		t.Fatal("range response changed")
	}
	r := <-got
	if r.Host != req.Host || r.RequestURI != "/media/a%2Fb?token=opaque%2Bvalue&x=1&x=2" || r.Header.Get("Range") != "bytes=2-4" || r.Header.Get("Authorization") != "Bearer playback-token" {
		t.Fatal("request changed")
	}
	if r.Header.Get("X-Silo-Ingress-Token") != "host-token" || len(r.Header.Values("X-Silo-Ingress-Token")) != 1 || r.Header.Get("X-Forwarded-Proto") != "https" {
		t.Fatal("ingress metadata spoofed")
	}
	if r.Header.Get("X-Forwarded-For") != "127.0.0.1" || r.Header.Get("Forwarded") != "" || r.Header.Get("X-Forwarded-Host") != "" || r.Header.Get("X-Real-IP") != "" {
		t.Fatal("forwarded identity spoofed")
	}
}

func TestWebSocketUpgradeAndDisconnectClosesTunnel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		_ = rw.Flush()
		_, _ = io.Copy(conn, conn)
	}))
	defer upstream.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := serve(t.Context(), ln, nil, strings.TrimPrefix(upstream.URL, "http://"), "token", make(chan error, 1))
	defer s.close()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	_, _ = fmt.Fprintf(conn, "GET /events HTTP/1.1\r\nHost: silo.example.test\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, &http.Request{Method: "GET"})
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("upgrade: %v %v", resp, err)
	}
	_, _ = conn.Write([]byte("ping"))
	b := make([]byte, 4)
	if _, err := io.ReadFull(r, b); err != nil || string(b) != "ping" {
		t.Fatal("tunnel does not pass data")
	}
	s.close()
	if _, err := r.ReadByte(); err == nil {
		t.Fatal("disconnected tunnel still open")
	} else if e, ok := err.(net.Error); ok && e.Timeout() {
		t.Fatal("tunnel not closed before deadline")
	}
}

func TestProxyFlushesEvents(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: ready\n\n")
		w.(http.Flusher).Flush()
		<-release
	}))
	defer upstream.Close()
	defer close(release)
	proxy, transport := NewProxy(strings.TrimPrefix(upstream.URL, "http://"), "token")
	defer transport.CloseIdleConnections()
	front := httptest.NewServer(proxy)
	defer front.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", front.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || line != "data: ready\n" {
		t.Fatalf("event buffered: %q %v", line, err)
	}
}
