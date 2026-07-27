// Command rcon runs commands on a Source RCON server.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/USA-RedDragon/rcon/internal/cli"
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

	return cli.MainContext(ctx, version, commit)
}
