package tailscale

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimehost"
	"tailscale.com/health"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
)

type watchResult struct {
	notification ipn.Notify
	err          error
}

type fakeWatcher struct {
	ctx    context.Context
	events chan watchResult
	done   chan struct{}
	once   sync.Once
}

func (w *fakeWatcher) Next() (ipn.Notify, error) {
	select {
	case event := <-w.events:
		return event.notification, event.err
	case <-w.done:
		return ipn.Notify{}, net.ErrClosed
	case <-w.ctx.Done():
		return ipn.Notify{}, w.ctx.Err()
	}
}

func (w *fakeWatcher) Close() error {
	w.once.Do(func() { close(w.done) })
	return nil
}

type fakeOverlay struct {
	mu           sync.Mutex
	current      *ipnstate.Status
	statusError  error
	startError   error
	watchError   error
	certificate  func(context.Context, string) ([]byte, []byte, error)
	failListenAt int
	listeners    []net.Listener
	ports        []string
	public       []bool
	closed       bool
	watcher      *fakeWatcher
}

func newFakeOverlay() *fakeOverlay {
	return &fakeOverlay{
		current: &ipnstate.Status{BackendState: "NeedsLogin", AuthURL: "https://login.example.test/private"},
		watcher: &fakeWatcher{events: make(chan watchResult, 8), done: make(chan struct{})},
		certificate: func(context.Context, string) ([]byte, []byte, error) {
			return []byte("certificate"), []byte("key"), nil
		},
	}
}

func (n *fakeOverlay) Start() error { return n.startError }
func (n *fakeOverlay) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.closed = true
	for _, listener := range n.listeners {
		_ = listener.Close()
	}
	return n.watcher.Close()
}
func (n *fakeOverlay) Status(context.Context) (*ipnstate.Status, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.current, n.statusError
}
func (n *fakeOverlay) CertPair(ctx context.Context, hostname string) ([]byte, []byte, error) {
	return n.certificate(ctx, hostname)
}
func (n *fakeOverlay) Watch(ctx context.Context) (notificationWatcher, error) {
	n.watcher.ctx = ctx
	return n.watcher, n.watchError
}
func (n *fakeOverlay) ListenTLS(_ context.Context, address string, _ *tls.Config, public bool) (net.Listener, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.ports = append(n.ports, address)
	n.public = append(n.public, public)
	if len(n.ports) == n.failListenAt {
		return nil, errors.New("private listener error")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err == nil {
		n.listeners = append(n.listeners, listener)
	}
	return listener, err
}
func (n *fakeOverlay) changeStatus(st *ipnstate.Status) {
	n.mu.Lock()
	n.current = st
	n.mu.Unlock()
	n.watcher.events <- watchResult{}
}

func runningStatus(hostname string) *ipnstate.Status {
	return &ipnstate.Status{
		BackendState: "Running", TailscaleIPs: []netip.Addr{netip.MustParseAddr("100.64.0.1")},
		Self:           &ipnstate.PeerStatus{DNSName: hostname + "."},
		CurrentTailnet: &ipnstate.TailnetStatus{MagicDNSEnabled: true}, CertDomains: []string{hostname},
	}
}

type listenerHost struct {
	*memoryHost
	info *runtimehost.HostInfo
}

func (h listenerHost) GetHostInfo(context.Context) (*runtimehost.HostInfo, error) { return h.info, nil }

type overlayRun struct {
	statuses chan *pluginv1.NetworkAccessStatus
	done     chan struct{}
	err      error
	cancel   context.CancelFunc
}

func startFakeOverlay(t *testing.T, node *fakeOverlay, allListeners bool) *overlayRun {
	return startConfiguredOverlay(t, node, allListeners, Config{})
}
func startConfiguredOverlay(t *testing.T, node *fakeOverlay, allListeners bool, config Config) *overlayRun {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	run := &overlayRun{statuses: make(chan *pluginv1.NetworkAccessStatus, 32), done: make(chan struct{}), cancel: cancel}
	h := newHost()
	info, _ := h.GetHostInfo(ctx)
	if allListeners {
		info.Listeners = append(info.Listeners,
			runtimehost.HostListener{Name: "jellyfin", Address: "127.0.0.1:8096"},
			runtimehost.HostListener{Name: "abs", Address: "127.0.0.1:13378"})
	}
	go func() {
		defer close(run.done)
		run.err = runOverlay(ctx, listenerHost{h, info}, config, func(s *pluginv1.NetworkAccessStatus) {
			select {
			case run.statuses <- s:
			case <-ctx.Done():
			}
		}, func(*runtimehost.HostInfo) overlay { return node }, nil)
	}()
	t.Cleanup(func() { cancel(); _ = run.wait(t) })
	return run
}

func (r *overlayRun) wait(t *testing.T) error {
	t.Helper()
	select {
	case <-r.done:
		return r.err
	case <-time.After(5 * time.Second):
		t.Fatal("overlay did not stop")
		return nil
	}
}
func (r *overlayRun) state(t *testing.T, wanted string) *pluginv1.NetworkAccessStatus {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case s := <-r.statuses:
			if s.State == wanted {
				return s
			}
		case <-r.done:
			t.Fatalf("overlay stopped before %s: %v", wanted, r.err)
		case <-deadline.C:
			t.Fatalf("no %s status", wanted)
		}
	}
}

func TestOverlayEnrollmentAndListenerRecovery(t *testing.T) {
	node := newFakeOverlay()
	run := startFakeOverlay(t, node, true)
	if s := run.state(t, "awaiting_authorization"); s.AuthUrl == "" || s.Origin != "" {
		t.Fatal(s)
	}
	node.changeStatus(runningStatus("silo.example.test"))
	if s := run.state(t, "connecting"); s.AuthUrl != "" || s.Origin != "" {
		t.Fatal("authorization URL survived enrollment")
	}
	if s := run.state(t, "connected"); len(s.Listeners) != 3 || s.Origin != "https://silo.example.test" {
		t.Fatal(s)
	}
	node.mu.Lock()
	firstListener := node.listeners[0]
	if strings.Join(node.ports, ",") != ":443,:8096,:13378" {
		t.Errorf("ports: %v", node.ports)
	}
	node.mu.Unlock()
	node.changeStatus(&ipnstate.Status{BackendState: "Starting"})
	if s := run.state(t, "connecting"); len(s.Listeners) != 0 || s.Origin != "" {
		t.Fatal("lost backend retained origins")
	}
	if _, err := firstListener.Accept(); err == nil {
		t.Fatal("lost backend retained listener")
	}
	node.changeStatus(runningStatus("renamed.example.test"))
	if s := run.state(t, "connected"); s.Hostname != "renamed.example.test" {
		t.Fatal("stale hostname")
	}
	run.cancel()
	if err := run.wait(t); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	node.mu.Lock()
	defer node.mu.Unlock()
	if !node.closed || len(node.ports) != 6 {
		t.Fatal("overlay lifecycle was not completed")
	}
}

func TestOverlayEnrollmentFailureIsSafeAndCanRecover(t *testing.T) {
	node := newFakeOverlay()
	node.current.AuthURL = ""
	run := startFakeOverlay(t, node, false)
	run.state(t, "connecting")
	secret := "rejected tskey-auth-private at https://login.example.test/private"
	node.watcher.events <- watchResult{notification: ipn.Notify{ErrMessage: &secret}}
	s := run.state(t, "error")
	if strings.Contains(s.Error, "private") || s.AuthUrl != "" || s.Origin != "" {
		t.Fatal("upstream error escaped")
	}
	node.watcher.events <- watchResult{notification: ipn.Notify{Health: &health.State{}}}
	run.state(t, "connecting")
	node.changeStatus(runningStatus("silo.example.test"))
	run.state(t, "connected")
}

func TestOverlayFailureClosesPartialListeners(t *testing.T) {
	for _, failure := range []string{"start", "watch", "second-listener", "status", "watch-stream"} {
		t.Run(failure, func(t *testing.T) {
			node := newFakeOverlay()
			node.current = runningStatus("silo.example.test")
			secret := errors.New("private failure with credentials")
			switch failure {
			case "start":
				node.startError = secret
			case "watch":
				node.watchError = secret
			case "second-listener":
				node.failListenAt = 2
			case "status":
				node.statusError = secret
			case "watch-stream":
				node.watcher.events <- watchResult{err: secret}
			}
			run := startFakeOverlay(t, node, true)
			err := run.wait(t)
			if err == nil || strings.Contains(err.Error(), "private") {
				t.Fatalf("unsafe error: %v", err)
			}
			node.mu.Lock()
			defer node.mu.Unlock()
			if failure != "start" && !node.closed {
				t.Fatal("overlay left running")
			}
			for _, listener := range node.listeners {
				if _, err := listener.Accept(); err == nil {
					t.Fatal("partial listener left open")
				}
			}
		})
	}
}

func TestCertificateLookupCanceledWithProviderOrHandshake(t *testing.T) {
	for _, cause := range []string{"provider", "handshake"} {
		t.Run(cause, func(t *testing.T) {
			node := newFakeOverlay()
			called := make(chan struct{})
			node.certificate = func(ctx context.Context, _ string) ([]byte, []byte, error) {
				close(called)
				<-ctx.Done()
				return nil, nil, ctx.Err()
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			handshake, stopHandshake := context.WithCancel(t.Context())
			defer stopHandshake()
			serverConn, clientConn := net.Pipe()
			defer func() { _ = serverConn.Close() }()
			defer func() { _ = clientConn.Close() }()
			server := tls.Server(serverConn, &tls.Config{GetCertificate: certificateFor(ctx, node, "silo.example.test"), MinVersion: tls.VersionTLS12})
			client := tls.Client(clientConn, &tls.Config{ServerName: "SILO.EXAMPLE.TEST", InsecureSkipVerify: true}) // DNS names are case-insensitive; this test cancels before verification.
			done := make(chan error, 1)
			go func() { done <- server.HandshakeContext(handshake) }()
			go func() { _ = client.HandshakeContext(handshake) }()
			select {
			case <-called:
			case <-time.After(5 * time.Second):
				t.Fatal("no certificate lookup")
			}
			if cause == "provider" {
				cancel()
			} else {
				stopHandshake()
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("canceled handshake succeeded")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("certificate work survived cancellation")
			}
		})
	}
}

func TestCertificateRejectsOtherNamesBeforeIssuance(t *testing.T) {
	node := newFakeOverlay()
	node.certificate = func(context.Context, string) ([]byte, []byte, error) {
		t.Fatal("issued certificate for another name")
		return nil, nil, nil
	}
	getter := certificateFor(t.Context(), node, "silo.example.test")
	for _, name := range []string{"", "other.example.test", "../../escape"} {
		if _, err := getter(&tls.ClientHelloInfo{ServerName: name}); err == nil {
			t.Fatalf("accepted SNI %q", name)
		}
	}
}

func TestCertificateErrorsDoNotExposePrivateDetails(t *testing.T) {
	cases := []struct {
		cause error
		want  string
	}{
		{context.DeadlineExceeded, "timed out"},
		{errors.New("429 rate limited secret-token"), "rate limited"},
		{errors.New("SetDNS private-challenge secret-token"), "DNS challenge failed"},
		{errors.New("cannot save encrypted overlay state secret-token"), "storage failed"},
		{errors.New("acme.Register secret-token"), "registration failed"},
		{errors.New("x509 secret-token"), "verification failed"},
		{errors.New("private error secret-token"), "issuance failed"},
	}
	for _, test := range cases {
		got := certificateError(test.cause).Error()
		if !strings.Contains(got, test.want) || strings.Contains(got, "secret-token") {
			t.Fatalf("unsafe or unhelpful error: %s", got)
		}
	}
}

func TestCertificateRetryKeepsIdentityWithoutAdvertisingOrigin(t *testing.T) {
	node := newFakeOverlay()
	attempts := 0
	node.certificate = func(context.Context, string) ([]byte, []byte, error) {
		attempts++
		if attempts < 3 {
			return nil, nil, errors.New("private challenge")
		}
		return []byte("cert"), []byte("key"), nil
	}
	var delays []time.Duration
	ready := &pluginv1.NetworkAccessStatus{Hostname: "silo.example.test", Addresses: []string{"100.64.0.1"}, Origin: "https://silo.example.test"}
	err := obtainCertificate(t.Context(), node, ready, func(s *pluginv1.NetworkAccessStatus) {
		if s.State != "error" || s.Hostname != ready.Hostname || s.Origin != "" || len(s.Listeners) != 0 || strings.Contains(s.Error, "private") {
			t.Fatalf("bad retry status: %v", s)
		}
	}, func(_ context.Context, d time.Duration) error { delays = append(delays, d); return nil })
	if err != nil || attempts != 3 || len(delays) != 2 || delays[0] != 15*time.Second || delays[1] != 30*time.Second {
		t.Fatalf("attempts=%d delays=%v err=%v", attempts, delays, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := waitRetry(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestFunnelOnlyExposesNativeAPIWhenExplicitlyEnabled(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			node := newFakeOverlay()
			node.current = runningStatus("silo.example.test")
			run := startConfiguredOverlay(t, node, true, Config{Funnel: enabled})
			run.state(t, "connected")
			node.mu.Lock()
			defer node.mu.Unlock()
			if len(node.public) != 3 || node.public[0] != enabled || node.public[1] || node.public[2] {
				t.Fatalf("public listeners: %v", node.public)
			}
		})
	}
}
