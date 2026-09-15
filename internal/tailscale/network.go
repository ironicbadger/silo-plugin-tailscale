package tailscale

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimehost"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

func validateHost(info *runtimehost.HostInfo) error {
	if info == nil || info.IngressToken == "" || info.Listener("api") == nil {
		return &PublicError{"host does not supply the network access contract"}
	}
	if info.HostRole != "api" && info.HostRole != "proxy" || info.HostRole == "proxy" && info.NodeID <= 0 {
		return &PublicError{"invalid host role or node identity"}
	}
	ports, names := map[int]bool{}, map[string]bool{}
	for _, l := range info.Listeners {
		if l.Name != "api" && l.Name != "jellyfin" && l.Name != "abs" || info.HostRole == "proxy" && l.Name != "api" {
			return &PublicError{"unsupported host listener"}
		}
		addr, err := netip.ParseAddrPort(l.Address)
		if err != nil || !addr.Addr().IsLoopback() || addr.Port() == 0 {
			return &PublicError{"host listener must use a loopback IP and port"}
		}
		port := listenerPort(l)
		if port < 1 || port > 65535 || ports[port] || names[l.Name] {
			return &PublicError{"duplicate or invalid overlay listener port or name"}
		}
		ports[port], names[l.Name] = true, true
	}
	return nil
}

func listenerPort(l runtimehost.HostListener) int {
	if l.DefaultPort != 0 {
		return l.DefaultPort
	}
	switch l.Name {
	case "jellyfin":
		return 8096
	case "abs":
		return 13378
	default:
		return 443
	}
}

func connectedStatus(info *runtimehost.HostInfo, st *ipnstate.Status) (*pluginv1.NetworkAccessStatus, error) {
	if st.Self == nil || st.CurrentTailnet == nil || !st.CurrentTailnet.MagicDNSEnabled {
		return nil, &PublicError{"enable MagicDNS in the Tailscale admin console"}
	}
	hostname := strings.TrimSuffix(st.Self.DNSName, ".")
	if hostname == "" || !slices.Contains(st.CertDomains, hostname) {
		return nil, &PublicError{"enable HTTPS certificates in the Tailscale admin console"}
	}
	s := &pluginv1.NetworkAccessStatus{State: "connected", Hostname: hostname}
	for _, ip := range st.TailscaleIPs {
		s.Addresses = append(s.Addresses, ip.String())
	}
	for _, l := range info.Listeners {
		origin := "https://" + hostname
		if port := listenerPort(l); port != 443 {
			origin = fmt.Sprintf("%s:%d", origin, port)
		}
		s.Listeners = append(s.Listeners, &pluginv1.NetworkAccessListener{Name: l.Name, Origin: origin})
		if l.Name == "api" {
			s.Origin = origin
		}
	}
	return s, nil
}

func Run(ctx context.Context, host Host, config Config, publish func(*pluginv1.NetworkAccessStatus)) error {
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	call, cancel := context.WithTimeout(ctx, 10*time.Second)
	info, err := host.GetHostInfo(call)
	cancel()
	if err != nil {
		return &PublicError{"cannot read host listeners; reconnect to retry"}
	}
	if err := validateHost(info); err != nil {
		return err
	}
	srv := &tsnet.Server{Hostname: Hostname(config, info), Store: StateStore{host}, NoLocalState: true,
		AuthKey: config.AuthKey, UserLogf: func(string, ...any) {}, Logf: func(string, ...any) {}}
	if err := srv.Start(); err != nil {
		return &PublicError{"cannot start tsnet; check encrypted state storage and retry"}
	}
	// Close is legal only after Start completes. It interrupts active network
	// operations and listeners on cancellation without deleting the identity.
	closed := make(chan struct{})
	go func() { <-ctx.Done(); _ = srv.Close(); close(closed) }()
	defer func() { stop(); <-closed }()
	lc, err := srv.LocalClient()
	if err != nil {
		return &PublicError{"cannot open tsnet control client"}
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	failures := make(chan error, len(info.Listeners))
	var servers []serving
	closeServers := func() {
		for _, s := range servers {
			s.close()
		}
		servers = nil
	}
	defer closeServers()
	lastHostname := ""
	for {
		call, cancel := context.WithTimeout(ctx, 5*time.Second)
		st, err := lc.StatusWithoutPeers(call)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return &PublicError{"cannot read tsnet status; reconnect to retry"}
		}
		if st.BackendState != "Running" || len(st.TailscaleIPs) == 0 {
			closeServers()
			s := &pluginv1.NetworkAccessStatus{State: "connecting"}
			if st.AuthURL != "" {
				s.State, s.AuthUrl = "awaiting_authorization", st.AuthURL
			}
			if st.BackendState == "NeedsMachineAuth" {
				s.State = "awaiting_authorization"
			}
			publish(s)
		} else {
			s, err := connectedStatus(info, st)
			if err != nil {
				return err
			}
			if len(servers) != 0 && s.Hostname != lastHostname {
				closeServers()
			}
			if len(servers) == 0 {
				// Enrollment is complete; discard the auth URL while HTTPS
				// issuance and listener setup are still in progress.
				publish(&pluginv1.NetworkAccessStatus{State: "connecting", Hostname: s.Hostname, Addresses: s.Addresses})
				call, cancel := context.WithTimeout(ctx, 2*time.Minute)
				_, _, err := lc.CertPair(call, s.Hostname)
				cancel()
				if err != nil {
					return &PublicError{"cannot obtain HTTPS certificate; check tailnet HTTPS settings and reconnect"}
				}
				for _, l := range info.Listeners {
					ln, err := srv.Listen("tcp", fmt.Sprintf(":%d", listenerPort(l)))
					if err != nil {
						return &PublicError{"cannot open all overlay listeners; reconnect to retry"}
					}
					tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: lc.GetCertificate}
					servers = append(servers, serve(ctx, ln, tlsConfig, l.Address, info.IngressToken, failures))
				}
				lastHostname = s.Hostname
			}
			publish(s)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-failures:
			return &PublicError{"overlay listener stopped; reconnect to retry"}
		case <-ticker.C:
		}
	}
}
