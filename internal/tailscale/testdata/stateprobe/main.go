// stateprobe exercises tsnet's production initialization without Go's test flags.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"tailscale.com/envknob"
	"tailscale.com/ipn/store/mem"
	"tailscale.com/logtail"
	"tailscale.com/net/netns"
	"tailscale.com/tsnet"
	"tailscale.com/types/logger"
	"tailscale.com/util/testenv"
)

// Wrapping mem.Store exercises the custom-store path used by the plugin.
// tsnet treats a bare *mem.Store specially and requires an ephemeral node.
type stateStore struct{ mem.Store }

func run() string {
	if len(os.Args) != 3 {
		return "expected control URL and state directory"
	}
	if testenv.InTest() {
		return "production state probe unexpectedly has test flags"
	}
	// A broken NoLocalState guard must expose local writes without uploading
	// diagnostic data from this regression test.
	logtail.Disable()
	envknob.SetNoLogsNoSupport()
	netns.SetEnabled(false)
	store := new(stateStore)
	server := &tsnet.Server{
		Hostname: "silo-production-state-probe", ControlURL: os.Args[1],
		AuthKey: "test-key", Dir: os.Args[2], Store: store, NoLocalState: true,
		UserLogf: logger.Discard, Logf: logger.Discard,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := server.Up(ctx); err != nil {
		_ = server.Close()
		return "production tsnet startup failed"
	}
	if err := server.Close(); err != nil {
		return "production tsnet shutdown failed"
	}
	if identity, err := store.ReadState("_machinekey"); err != nil || len(identity) == 0 {
		return "production tsnet did not persist machine identity"
	}
	return ""
}

func main() {
	if message := run(); message != "" {
		fmt.Fprintln(os.Stderr, message)
		os.Exit(1)
	}
}
