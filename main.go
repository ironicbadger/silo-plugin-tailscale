// Command silo-plugin-tailscale serves the resident Tailscale provider over
// Silo's private go-plugin transport. Silo owns its process lifecycle.
package main

import (
	"context"
	_ "embed"
	"os"
	"sync"

	pluginv1 "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginproto/silo/plugin/v1"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/manifest"
	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
	"github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtimedefault"
	"github.com/ironicbadger/silo-plugin-tailscale/internal/tailscale"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"tailscale.com/envknob"
	"tailscale.com/logtail"
)

//go:embed manifest.json
var manifestJSON []byte
var version = "0.1.0"

type plugin struct {
	runtimedefault.Server
	pluginv1.UnimplementedNetworkAccessProviderServer
	mu       sync.RWMutex
	manifest *pluginv1.PluginManifest
	provider *tailscale.Provider
}

func (p *plugin) GetManifest(context.Context, *pluginv1.GetManifestRequest) (*pluginv1.GetManifestResponse, error) {
	return &pluginv1.GetManifestResponse{Manifest: p.manifest}, nil
}
func (p *plugin) Configure(ctx context.Context, req *pluginv1.ConfigureRequest) (*pluginv1.ConfigureResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	config, err := tailscale.ParseConfig(req.GetConfig())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	host := sdkruntime.Host()
	if host == nil {
		return nil, status.Error(codes.FailedPrecondition, "host broker is not bound")
	}
	if p.provider != nil {
		p.provider.Close()
		p.provider = nil
	}
	provider := tailscale.New(host, config, nil)
	if err := provider.Restore(ctx); err != nil {
		provider.Close()
		return nil, err
	}
	p.provider = provider
	return &pluginv1.ConfigureResponse{}, nil
}
func (p *plugin) Connect(ctx context.Context, req *pluginv1.NetworkAccessConnectRequest) (*pluginv1.NetworkAccessStatus, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.provider == nil {
		return nil, status.Error(codes.FailedPrecondition, "plugin is not configured")
	}
	return p.provider.Connect(ctx, req)
}
func (p *plugin) Disconnect(ctx context.Context, req *pluginv1.NetworkAccessDisconnectRequest) (*pluginv1.NetworkAccessStatus, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.provider == nil {
		return nil, status.Error(codes.FailedPrecondition, "plugin is not configured")
	}
	return p.provider.Disconnect(ctx, req)
}
func (p *plugin) GetStatus(ctx context.Context, req *pluginv1.NetworkAccessGetStatusRequest) (*pluginv1.NetworkAccessStatus, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.provider == nil {
		return nil, status.Error(codes.FailedPrecondition, "plugin is not configured")
	}
	return p.provider.GetStatus(ctx, req)
}
func main() {
	// Enrollment is controlled only by the encrypted Silo configuration.
	// Do not inherit daemon credentials or forced re-enrollment from the host.
	for _, key := range []string{"TS_AUTHKEY", "TS_CLIENT_SECRET", "TS_CLIENT_ID", "TS_ID_TOKEN", "TS_AUDIENCE", "TS_CONTROL_URL", "TSNET_FORCE_LOGIN"} {
		_ = os.Unsetenv(key)
	}
	// Disable diagnostic uploads as well as both tsnet log callbacks. Auth
	// URLs, keys, ingress tokens and upstream error strings never reach logs.
	envknob.SetNoLogsNoSupport()
	logtail.Disable()
	m, err := manifest.LoadWithChecksum(manifestJSON, version)
	if err != nil {
		panic(err)
	}
	p := &plugin{manifest: m}
	sdkruntime.Serve(sdkruntime.ServeConfig{Servers: sdkruntime.CapabilityServers{Runtime: p, NetworkAccessProvider: p}})
}
