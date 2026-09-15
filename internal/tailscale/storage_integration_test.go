package tailscale

import (
	"context"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"tailscale.com/net/netns"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
	"tailscale.com/tstest/integration"
	"tailscale.com/tstest/integration/testcontrol"
	"tailscale.com/types/logger"
)

// Exercise the real userspace stack against a local control server. No real
// tailnet, account, auth key, or external certificate authority is contacted.
func TestTSNetStoresIdentityWithoutLocalFiles(t *testing.T) {
	netns.SetEnabled(false)
	t.Cleanup(func() { netns.SetEnabled(true) })
	control := &testcontrol.Server{DERPMap: integration.RunDERPAndSTUN(t, logger.Discard, "127.0.0.1"), MagicDNSDomain: "example.test", DNSConfig: &tailcfg.DNSConfig{Proxied: true}}
	control.HTTPTestServer = httptest.NewServer(control)
	defer control.HTTPTestServer.Close()
	h := newHost()
	stateDir := t.TempDir()
	s := &tsnet.Server{Hostname: "silo-test", ControlURL: control.HTTPTestServer.URL, AuthKey: "test-key",
		Dir: stateDir, Store: StateStore{h}, NoLocalState: true, UserLogf: func(string, ...any) {}, Logf: func(string, ...any) {}}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	if _, err := s.Up(ctx); err != nil {
		_ = s.Close()
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("tsnet wrote local files: %v", entries)
	}
	if b, found, err := h.ReadInstanceState(ctx, "_machinekey"); err != nil || !found || len(b) == 0 {
		t.Fatal("machine identity missing from host store")
	}
}
