package tailscale

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"time"
)

// NewProxy replaces all client-supplied forwarding metadata. Rewrite runs
// after hop-by-hop headers are removed, so Connection cannot strip our token.
func NewProxy(address, token string) (*httputil.ReverseProxy, *http.Transport) {
	transport := &http.Transport{
		Proxy:        nil,
		DialContext:  (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns: 100, IdleConnTimeout: 90 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	p := &httputil.ReverseProxy{
		Transport:     transport,
		FlushInterval: -1,
		ErrorLog:      log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "Silo listener unavailable", http.StatusBadGateway)
		},
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(&url.URL{Scheme: "http", Host: address})
			r.Out.Host = r.In.Host
			r.Out.Header.Del("Forwarded")
			r.Out.Header.Del("X-Forwarded-Host")
			r.Out.Header.Del("X-Real-IP")
			peer, _, err := net.SplitHostPort(r.In.RemoteAddr)
			if err != nil {
				peer = r.In.RemoteAddr
			}
			r.Out.Header.Set("X-Forwarded-For", peer)
			r.Out.Header.Set("X-Forwarded-Proto", "https")
			r.Out.Header.Set("X-Silo-Ingress-Token", token)
		},
	}
	return p, transport
}

// Track hijacked WebSocket connections too: http.Server.Close alone leaves
// them alive, which would let a disconnected provider continue proxying.
type connections struct {
	mu  sync.Mutex
	all map[net.Conn]struct{}
}

type trackedConn struct {
	net.Conn
	owner *connections
}

func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.owner.mu.Lock()
	delete(c.owner.all, c)
	c.owner.mu.Unlock()
	return err
}

type trackedListener struct {
	net.Listener
	owner *connections
}

func (l trackedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tracked := &trackedConn{conn, l.owner}
	l.owner.mu.Lock()
	l.owner.all[tracked] = struct{}{}
	l.owner.mu.Unlock()
	return tracked, nil
}
func (c *connections) close() {
	c.mu.Lock()
	all := c.all
	c.all = make(map[net.Conn]struct{})
	c.mu.Unlock()
	for conn := range all {
		_ = conn.Close()
	}
}

type serving struct {
	server      *http.Server
	transport   *http.Transport
	connections *connections
}

func serve(ctx context.Context, listener net.Listener, tlsConfig *tls.Config, address, token string, failures chan<- error) serving {
	proxy, transport := NewProxy(address, token)
	connections := &connections{all: make(map[net.Conn]struct{})}
	listener = trackedListener{listener, connections}
	if tlsConfig != nil {
		listener = tls.NewListener(listener, tlsConfig)
	}
	server := &http.Server{Handler: proxy, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second,
		ErrorLog:    log.New(io.Discard, "", 0),
		BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() {
		err := server.Serve(listener)
		if !errors.Is(err, http.ErrServerClosed) {
			select {
			case failures <- err:
			default:
			}
		}
	}()
	return serving{server, transport, connections}
}
func (s serving) close() {
	_ = s.server.Close()
	s.connections.close()
	s.transport.CloseIdleConnections()
}
