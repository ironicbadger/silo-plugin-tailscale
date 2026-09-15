// Package tailscale implements Silo's resident network access provider.
package tailscale

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimehost"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const ProviderVersion = "tsnet 1.102.4"
const desiredKey = "desired_connected"

type Host interface {
	GetHostInfo(context.Context) (*runtimehost.HostInfo, error)
	ReadInstanceState(context.Context, string) ([]byte, bool, error)
	WriteInstanceState(context.Context, string, []byte) error
	ReportNetworkAccessStatus(context.Context, *pluginv1.NetworkAccessStatus) error
}

type Runner func(context.Context, Host, Config, func(*pluginv1.NetworkAccessStatus)) error

type Provider struct {
	pluginv1.UnimplementedNetworkAccessProviderServer
	host   Host
	config Config
	run    Runner
	life   sync.Mutex
	mu     sync.Mutex
	status *pluginv1.NetworkAccessStatus
	cancel context.CancelFunc
	done   chan struct{}
	wake   chan struct{}
	stop   context.CancelFunc
}

func New(host Host, config Config, run Runner) *Provider {
	if run == nil {
		run = Run
	}
	done := make(chan struct{})
	close(done)
	ctx, stop := context.WithCancel(context.Background())
	p := &Provider{host: host, config: config, run: run, done: done, wake: make(chan struct{}, 1), stop: stop,
		status: &pluginv1.NetworkAccessStatus{State: "disconnected", ProviderVersion: ProviderVersion}}
	go p.report(ctx)
	return p
}

// Restore is called during Configure, after the host broker is bound. It never
// waits for enrollment and does not need a subsequent admin RPC to reconnect.
func (p *Provider) Restore(ctx context.Context) error {
	p.life.Lock()
	defer p.life.Unlock()
	b, found, err := p.host.ReadInstanceState(ctx, desiredKey)
	if err != nil {
		return status.Error(codes.Unavailable, "cannot read connection intent")
	}
	if found && string(b) != "0" && string(b) != "1" {
		return status.Error(codes.FailedPrecondition, "invalid saved connection intent")
	}
	if string(b) == "1" {
		p.start()
	} else {
		p.signal()
	}
	return nil
}

func (p *Provider) Connect(ctx context.Context, _ *pluginv1.NetworkAccessConnectRequest) (*pluginv1.NetworkAccessStatus, error) {
	p.life.Lock()
	defer p.life.Unlock()
	if p.cancel != nil {
		select {
		case <-p.done:
			p.cancel()
			p.cancel = nil
		default:
			return p.snapshot(), nil
		}
	}
	select {
	case <-p.done:
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	}
	if err := p.host.WriteInstanceState(ctx, desiredKey, []byte("1")); err != nil {
		return nil, status.Error(codes.Unavailable, "cannot save connection intent")
	}
	p.start()
	return p.snapshot(), nil
}

func (p *Provider) start() {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.done = make(chan struct{})
	p.set(&pluginv1.NetworkAccessStatus{State: "connecting", DesiredConnected: true})
	done := p.done
	go func() {
		defer close(done)
		publish := func(s *pluginv1.NetworkAccessStatus) {
			p.mu.Lock()
			defer p.mu.Unlock()
			if ctx.Err() != nil {
				return
			}
			s.DesiredConnected = true
			p.setLocked(s)
		}
		err := p.run(ctx, p.host, p.config, publish)
		if ctx.Err() == nil {
			message := "overlay stopped unexpectedly; reconnect to retry"
			var safe *PublicError
			if errors.As(err, &safe) {
				message = safe.Message
			}
			publish(&pluginv1.NetworkAccessStatus{State: "error", Error: message})
		}
	}()
}

func (p *Provider) Disconnect(ctx context.Context, _ *pluginv1.NetworkAccessDisconnectRequest) (*pluginv1.NetworkAccessStatus, error) {
	p.life.Lock()
	defer p.life.Unlock()
	if err := p.host.WriteInstanceState(ctx, desiredKey, []byte("0")); err != nil {
		return nil, status.Error(codes.Unavailable, "cannot save connection intent")
	}
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	p.set(&pluginv1.NetworkAccessStatus{State: "disconnected"})
	select {
	case <-p.done:
		return p.snapshot(), nil
	case <-ctx.Done():
		return nil, status.FromContextError(ctx.Err()).Err()
	}
}

func (p *Provider) GetStatus(context.Context, *pluginv1.NetworkAccessGetStatusRequest) (*pluginv1.NetworkAccessStatus, error) {
	return p.snapshot(), nil
}

// Close stops the process without changing the persisted operator intent.
func (p *Provider) Close() {
	p.life.Lock()
	defer p.life.Unlock()
	p.stop()
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	<-p.done
}

func (p *Provider) snapshot() *pluginv1.NetworkAccessStatus {
	p.mu.Lock()
	defer p.mu.Unlock()
	return proto.Clone(p.status).(*pluginv1.NetworkAccessStatus)
}

func (p *Provider) set(s *pluginv1.NetworkAccessStatus) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.setLocked(s)
}
func (p *Provider) setLocked(s *pluginv1.NetworkAccessStatus) {
	s.ProviderVersion = ProviderVersion
	if proto.Equal(p.status, s) {
		return
	}
	if p.status.State != s.State {
		slog.Info("network access state changed", "state", s.State)
	}
	p.status = proto.Clone(s).(*pluginv1.NetworkAccessStatus)
	p.signal()
}
func (p *Provider) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// One reporter preserves ordering; periodic retries repair missed status pushes.
func (p *Provider) report(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.wake:
		case <-ticker.C:
		}
		call, cancel := context.WithTimeout(ctx, 3*time.Second)
		err := p.host.ReportNetworkAccessStatus(call, p.snapshot())
		cancel()
		if err != nil && ctx.Err() == nil {
			slog.Warn("network access status delivery failed")
		}
	}
}

// PublicError contains only fixed, operator-safe text, never upstream errors.
type PublicError struct{ Message string }

func (e *PublicError) Error() string { return e.Message }
