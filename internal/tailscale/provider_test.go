package tailscale

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimehost"
	"tailscale.com/ipn"
)

type memoryHost struct {
	mu                  sync.Mutex
	values              map[string][]byte
	failRead, failWrite bool
	reports             chan *pluginv1.NetworkAccessStatus
}

func newHost() *memoryHost {
	return &memoryHost{values: map[string][]byte{}, reports: make(chan *pluginv1.NetworkAccessStatus, 100)}
}
func (h *memoryHost) GetHostInfo(context.Context) (*runtimehost.HostInfo, error) {
	return &runtimehost.HostInfo{HostRole: "api", IngressToken: "test-token", Listeners: []runtimehost.HostListener{{Name: "api", Address: "127.0.0.1:8080", DefaultPort: 443}}}, nil
}
func (h *memoryHost) ReadInstanceState(_ context.Context, key string) ([]byte, bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failRead {
		return nil, false, errors.New("secret upstream failure")
	}
	b, ok := h.values[key]
	return append([]byte{}, b...), ok, nil
}
func (h *memoryHost) WriteInstanceState(_ context.Context, key string, b []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failWrite {
		return errors.New("secret upstream failure")
	}
	h.values[key] = append([]byte{}, b...)
	return nil
}
func (h *memoryHost) ReportNetworkAccessStatus(_ context.Context, s *pluginv1.NetworkAccessStatus) error {
	h.reports <- s
	return nil
}
func waitState(t *testing.T, h *memoryHost, state string) *pluginv1.NetworkAccessStatus {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case s := <-h.reports:
			if s.State == state {
				return s
			}
		case <-timer.C:
			t.Fatalf("no %s status", state)
			return nil
		}
	}
}

func TestRestoreAndDisconnectPreserveIdentity(t *testing.T) {
	h := newHost()
	h.values[desiredKey] = []byte("1")
	h.values["node"] = []byte("identity")
	started := make(chan struct{}, 1)
	runner := func(ctx context.Context, _ Host, _ Config, publish func(*pluginv1.NetworkAccessStatus)) error {
		started <- struct{}{}
		publish(&pluginv1.NetworkAccessStatus{State: "awaiting_authorization", AuthUrl: "https://login.example.test/secret"})
		<-ctx.Done()
		// A late result must never resurrect a disconnected provider.
		publish(&pluginv1.NetworkAccessStatus{State: "connected", Origin: "https://stale.invalid"})
		return ctx.Err()
	}
	p := New(h, Config{}, runner)
	defer p.Close()
	if err := p.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("restart did not reconnect")
	}
	s := waitState(t, h, "awaiting_authorization")
	if s.AuthUrl == "" || !s.DesiredConnected {
		t.Fatal(s)
	}
	if _, err := p.Connect(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	s, err := p.Disconnect(t.Context(), nil)
	if err != nil || s.State != "disconnected" || s.DesiredConnected || s.AuthUrl != "" || s.Origin != "" {
		t.Fatalf("%v %v", s, err)
	}
	waitState(t, h, "disconnected")
	if b, _, _ := h.ReadInstanceState(t.Context(), desiredKey); string(b) != "0" {
		t.Fatal("intent not persisted")
	}
	if b, _, _ := h.ReadInstanceState(t.Context(), "node"); string(b) != "identity" {
		t.Fatal("identity lost")
	}
}

func TestPersistenceFailuresDoNotChangeIntent(t *testing.T) {
	h := newHost()
	p := New(h, Config{}, func(ctx context.Context, _ Host, _ Config, _ func(*pluginv1.NetworkAccessStatus)) error {
		<-ctx.Done()
		return ctx.Err()
	})
	defer p.Close()
	h.failRead = true
	if err := p.Restore(t.Context()); err == nil {
		t.Fatal("ignored failed state read")
	}
	h.failRead = false
	h.failWrite = true
	if _, err := p.Connect(t.Context(), nil); err == nil {
		t.Fatal("ignored failed save")
	}
	if p.snapshot().DesiredConnected {
		t.Fatal("changed intent despite failed write")
	}
	h.failWrite = false
	if _, err := p.Connect(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.failWrite = true
	h.mu.Unlock()
	if _, err := p.Disconnect(t.Context(), nil); err == nil {
		t.Fatal("ignored failed disconnect save")
	}
	if !p.snapshot().DesiredConnected {
		t.Fatal("changed intent despite failed write")
	}
}

func TestCanceledConnectRPCDoesNotCancelEnrollment(t *testing.T) {
	h := newHost()
	started := make(chan context.Context, 1)
	p := New(h, Config{}, func(ctx context.Context, _ Host, _ Config, _ func(*pluginv1.NetworkAccessStatus)) error {
		started <- ctx
		<-ctx.Done()
		return ctx.Err()
	})
	defer p.Close()
	ctx, cancel := context.WithCancel(t.Context())
	if _, err := p.Connect(ctx, nil); err != nil {
		t.Fatal(err)
	}
	cancel()
	runCtx := <-started
	if runCtx.Err() != nil {
		t.Fatal("enrollment tied to RPC lifetime")
	}
	p.Close()
	if b, _, _ := h.ReadInstanceState(t.Context(), desiredKey); string(b) != "1" {
		t.Fatal("process stop cleared operator intent")
	}
}

func TestUnknownErrorsDoNotExposeSecrets(t *testing.T) {
	h := newHost()
	p := New(h, Config{}, func(context.Context, Host, Config, func(*pluginv1.NetworkAccessStatus)) error {
		return errors.New("tskey-auth-secret https://login.example.test/secret")
	})
	defer p.Close()
	_, _ = p.Connect(t.Context(), nil)
	s := waitState(t, h, "error")
	if s.Error != "overlay stopped unexpectedly; reconnect to retry" || !s.DesiredConnected {
		t.Fatal(s)
	}
	copy := p.snapshot()
	copy.Error = "changed"
	if p.snapshot().Error == "changed" {
		t.Fatal("status shares mutable data")
	}
}

func TestStoreRoundTripAndIsolation(t *testing.T) {
	a, b := StateStore{newHost()}, StateStore{newHost()}
	if _, err := a.ReadState("missing"); !errors.Is(err, ipn.ErrStateNotExist) {
		t.Fatal(err)
	}
	for _, key := range []ipn.StateKey{"_machinekey", "profile-test", "acme-account.key.pem", "silo.example.test.key", "silo.example.test.crt"} {
		if err := a.WriteState(key, []byte("secret")); err != nil {
			t.Fatal(err)
		}
		got, err := a.ReadState(key)
		if err != nil || string(got) != "secret" {
			t.Fatalf("%s: %v", key, err)
		}
		if _, err := b.ReadState(key); !errors.Is(err, ipn.ErrStateNotExist) {
			t.Fatal("cross-instance state leak")
		}
	}
	if err := a.WriteState("empty", nil); err != nil {
		t.Fatal(err)
	}
	if got, err := a.ReadState("empty"); err != nil || len(got) != 0 {
		t.Fatal("empty value confused with missing")
	}
}
