package tailscale

import (
	"testing"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimehost"
	"google.golang.org/protobuf/types/known/structpb"
	"tailscale.com/ipn/ipnstate"
)

func TestConfigAndHostnames(t *testing.T) {
	c, err := ParseConfig(nil)
	if err != nil || c.HostnamePrefix != "silo" {
		t.Fatal(c, err)
	}
	for _, value := range []any{"UPPER", "bad.name", "-bad", "bad-", 42, ""} {
		v, _ := structpb.NewStruct(map[string]any{"hostname_prefix": value})
		if _, err := ParseConfig([]*pluginv1.ConfigEntry{{Key: "tailscale", Value: v}}); err == nil {
			t.Fatalf("accepted prefix %v", value)
		}
	}
	a := &runtimehost.HostInfo{HostRole: "api"}
	b := &runtimehost.HostInfo{HostRole: "proxy", NodeID: 1}
	d := &runtimehost.HostInfo{HostRole: "proxy", NodeID: 2}
	if Hostname(c, a) == Hostname(c, b) || Hostname(c, b) == Hostname(c, d) {
		t.Fatal("hosts share hostname")
	}
}

func TestAllListenersAndValidation(t *testing.T) {
	info, _ := newHost().GetHostInfo(t.Context())
	info.Listeners = append(info.Listeners, runtimehost.HostListener{Name: "jellyfin", Address: "127.0.0.1:8096"}, runtimehost.HostListener{Name: "abs", Address: "[::1]:13378"})
	if err := validateHost(info); err != nil {
		t.Fatal(err)
	}
	st := &ipnstate.Status{Self: &ipnstate.PeerStatus{DNSName: "silo.example.test."}, CurrentTailnet: &ipnstate.TailnetStatus{MagicDNSEnabled: true}, CertDomains: []string{"silo.example.test"}}
	s, err := connectedStatus(info, st)
	if err != nil || len(s.Listeners) != 3 || s.Origin != "https://silo.example.test" || s.Listeners[1].Origin != "https://silo.example.test:8096" || s.Listeners[2].Origin != "https://silo.example.test:13378" {
		t.Fatalf("%v %v", s, err)
	}
	st.CertDomains = nil
	if _, err := connectedStatus(info, st); err == nil {
		t.Fatal("HTTPS missing but ready")
	}
	for _, address := range []string{"192.0.2.1:8080", "example.test:8080", "127.0.0.1:0", "127.0.0.1"} {
		info.Listeners[0].Address = address
		if err := validateHost(info); err == nil {
			t.Fatalf("accepted %s", address)
		}
	}
	info.Listeners[0].Address = "127.0.0.1:8080"
	info.Listeners[1].DefaultPort = 443
	if err := validateHost(info); err == nil {
		t.Fatal("accepted colliding ports")
	}
}
