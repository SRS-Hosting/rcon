// Command rcon runs commands on a Source RCON server.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/SRS-Hosting/rcon/internal/cli"
)

// Stamped by the release build; see .goreleaser.yml.
//
//nolint:gochecknoglobals // ldflags cannot write anything else
var (
	version = "dev"
	commit  = "none"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// The first signal asks for a graceful exit through the cancelled context.
	// After that, restore default signal behavior so a second Ctrl-C force-kills
	// the process even if shutdown is wedged. stop is idempotent, so the
	// deferred call above remains correct.
	context.AfterFunc(ctx, stop)

	return cli.MainContext(ctx, version, commit)
}
