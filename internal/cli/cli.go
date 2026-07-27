// Package cli implements the rcon command-line tool.
package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/USA-RedDragon/configulator"
	"github.com/USA-RedDragon/rcon"
	"github.com/USA-RedDragon/rcon/internal/config"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Exit codes. These are a contract: a container lifecycle hook branches on
// success to decide whether to wait out a graceful restart or shut down now, so
// a failure that exited 0 would hold a terminating pod open for the whole drain
// every time.
const (
	// ExitOK reports that the command ran and the server answered in full.
	ExitOK = 0
	// ExitFailure reports that the command did not complete: no connection, a
	// rejected password, or an error from the server.
	ExitFailure = 1
	// ExitUsage reports a problem with the invocation itself, before anything was
	// sent.
	ExitUsage = 2
	// ExitTruncated reports that the server answered but the response was cut
	// short, so whether the command finished is genuinely unknown. It is separate
	// from ExitFailure so a caller can decide how much that uncertainty matters;
	// anything that only checks for zero treats it as the failure it might be.
	ExitTruncated = 3
)

// shorthands are the single-letter forms the deployed command line already uses.
//
// configulator derives flag names from the config struct and has no way to
// express a shorthand, so they are grafted on after registration rather than
// invented here. Changing these breaks existing callers.
//
//nolint:gochecknoglobals // a lookup table, never reassigned
var shorthands = map[string]string{
	"address":  "a",
	"password": "p",
	"host":     "H",
	"port":     "P",
}

// usageError marks a problem with the invocation rather than with the exchange,
// so Main can exit ExitUsage without treating every failure as misuse.
type usageError struct{ err error }

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

// New returns the root command, registering configuration flags onto it.
func New(version, commit string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rcon [flags] [command...]",
		Short: "Run a command on a Source RCON server",
		Long: `Runs a command on a Source RCON server and prints the response.

With a command, rcon runs it and exits, which is the form to use from scripts and
container lifecycle hooks. With no command it reads one command per line from
standard input until end of input, prompting when that is a terminal.

Settings come from a config file, then the environment, then flags, each
overriding the last. Environment variables are prefixed with ` + config.EnvPrefix +
			`, so the password is taken from ` + config.EnvPrefix + `PASSWORD. Prefer that over
--password: an argument is visible to anyone who can read the process list.

Exit codes: 0 success, 1 the command did not complete, 2 bad invocation,
3 the server answered but the response was cut short.`,
		Example: `  rcon -a 127.0.0.1:27015 -p secret status
  rcon -a game:7779 "Restart 600"
  RCON_PASSWORD=secret rcon -a game:7779 status
  echo status | rcon -a game:7779`,
		Version:       fmt.Sprintf("%s (%s)", version, commit),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          run,
	}

	// A rejected flag is a bad invocation, not a failed command, and the exit
	// code has to say so.
	cmd.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &usageError{err: fmt.Errorf("%w (try --help)", err)}
	})

	return cmd
}

// RegisterFlags binds cfg's flags onto cmd, adding the shorthands the deployed
// command line depends on.
//
// configulator registers into its own flag set, and the Flag values it creates
// are pointers, so adding those same pointers to the command means one parse
// updates both views. That is what lets the shorthands be attached here while
// configulator stays the thing that actually reads them.
func RegisterFlags(cmd *cobra.Command, loader *configulator.Configulator[config.Config]) error {
	set := pflag.NewFlagSet("config", pflag.ContinueOnError)
	loader.WithPFlags(set, nil)

	var errs []error
	set.VisitAll(func(f *pflag.Flag) {
		if short, ok := shorthands[f.Name]; ok {
			if f.Shorthand != "" && f.Shorthand != short {
				errs = append(errs, fmt.Errorf("flag %s already has shorthand %q", f.Name, f.Shorthand))
				return
			}
			f.Shorthand = short
		}
		cmd.Flags().AddFlag(f)
	})
	return errors.Join(errs...)
}

// MainContext runs the CLI and returns the process exit code.
func MainContext(ctx context.Context, version, commit string) int {
	return execute(ctx, New(version, commit))
}

// execute wires configuration onto cmd, runs it, and turns the outcome into an
// exit code.
func execute(ctx context.Context, cmd *cobra.Command) int {
	// WithFile before RegisterFlags, because configulator only adds the
	// --config flag when it knows there is a file to look for.
	loader := configulator.New[config.Config]().
		WithEnvironmentVariables(&configulator.EnvironmentVariableOptions{
			Prefix:    config.EnvPrefix,
			Separator: "_",
		}).
		WithFile(&configulator.FileOptions{Paths: []string{"config.yaml"}})

	if err := RegisterFlags(cmd, loader); err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), prefixed(err))
		return ExitUsage
	}
	cmd.SetContext(loader.WithContext(ctx))

	err := cmd.Execute()
	if err == nil {
		return ExitOK
	}

	fmt.Fprintln(cmd.ErrOrStderr(), prefixed(err))

	var usage *usageError
	switch {
	case errors.As(err, &usage):
		return ExitUsage
	case errors.Is(err, rcon.ErrTruncated):
		return ExitTruncated
	default:
		return ExitFailure
	}
}

// prefixed names the program in an error line without stuttering.
//
// Messages arrive from three places: this package, cobra, and the rcon library,
// whose errors already begin with "rcon: " because that is its package name. The
// prefix is added only where it is missing, so every line is attributable and
// none says it twice.
func prefixed(err error) string {
	const prefix = "rcon: "
	msg := err.Error()
	if strings.HasPrefix(msg, prefix) {
		return msg
	}
	return prefix + msg
}

func run(cmd *cobra.Command, args []string) error {
	loader, err := configulator.FromContext[config.Config](cmd.Context())
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}
	cfg, err := loader.Load()
	if err != nil {
		// Everything Load rejects is a problem with how the command was invoked,
		// not with the server.
		return &usageError{err: err}
	}

	client := rcon.New(cfg.Addr(), cfg.Password, rcon.WithTimeout(cfg.Timeout()))

	if len(args) == 0 {
		return interactive(cmd, client)
	}
	// Joined rather than requiring one quoted argument, so both `rcon "Restart
	// 600"` and `rcon Restart 600` do what they obviously mean.
	return once(cmd, client, strings.Join(args, " "))
}

// once runs a single command and prints its response.
func once(cmd *cobra.Command, client *rcon.Client, command string) error {
	output, err := client.Execute(cmd.Context(), command)

	// A truncated response still carries what arrived, so print it before
	// reporting the problem: partial output is usually the most informative thing
	// available when something goes wrong mid-response.
	writeOutput(cmd, output)
	return err
}

// interactive reads one command per line until end of input.
//
// This is also what makes `echo status | rcon -a ...` work, so a pipeline needs
// no special handling.
func interactive(cmd *cobra.Command, client *rcon.Client) error {
	in := cmd.InOrStdin()
	out := cmd.OutOrStdout()
	prompt := isTerminal(in)

	if prompt {
		fmt.Fprintf(out, "Connected to %s. Type a command, or exit.\n", client.Addr())
	}

	scanner := bufio.NewScanner(in)
	for {
		if prompt {
			fmt.Fprint(out, "> ")
		}
		if !scanner.Scan() {
			break
		}

		command := strings.TrimSpace(scanner.Text())
		switch command {
		case "":
			continue
		case "exit", "quit":
			return nil
		}

		output, err := client.Execute(cmd.Context(), command)
		writeOutput(cmd, output)
		if err != nil {
			fmt.Fprintln(cmd.ErrOrStderr(), prefixed(err))
			if !prompt {
				// Piped input is a script: carrying on after a failure would hide it
				// behind whatever the later commands did, and lose the exit code.
				return err
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read commands: %w", err)
	}
	return nil
}

func writeOutput(cmd *cobra.Command, output string) {
	if output == "" {
		return
	}
	fmt.Fprintln(cmd.OutOrStdout(), strings.TrimRight(output, "\n"))
}

// isTerminal reports whether r is an interactive terminal, which is what decides
// between prompting and behaving like a filter.
func isTerminal(r io.Reader) bool {
	file, ok := r.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
