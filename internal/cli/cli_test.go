package cli_test

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/USA-RedDragon/rcon/internal/cli"
	"github.com/USA-RedDragon/rcon/rcontest"
)

const testPassword = "swordfish"

// run invokes the CLI the way a shell would and reports what a caller sees.
func run(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()

	// Nothing in the ambient environment should reach the command. Setenv first
	// so the original value is restored afterwards, then actually clear it:
	// an empty-but-set variable is still a value configulator would apply, and
	// an empty host would override the default rather than leave it alone.
	for _, name := range []string{"RCON_ADDRESS", "RCON_HOST", "RCON_PORT", "RCON_PASSWORD", "RCON_TIMEOUTSECONDS"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
	}
	// A config.yaml in the working directory would also be picked up, so run
	// from an empty one.
	t.Chdir(t.TempDir())

	var out, errOut bytes.Buffer
	code = cli.RunForTest(t.Context(), args, strings.NewReader(stdin), &out, &errOut)
	return code, out.String(), errOut.String()
}

// serve starts a fake RCON server answering every command with response.
func serve(t *testing.T, password, response string) *rcontest.Server {
	t.Helper()
	srv, err := rcontest.New(rcontest.Respond(password, 0, func(string) string {
		return response
	}))
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(srv.Close)
	return srv
}

// TestOneShotSucceeds is the form a container lifecycle hook uses.
func TestOneShotSucceeds(t *testing.T) {
	srv := serve(t, testPassword, "Restarting in 600 seconds")

	code, stdout, stderr := run(t, "", "-a", srv.Addr(), "-p", testPassword, "Restart 600")

	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, cli.ExitOK, stderr)
	}
	if !strings.Contains(stdout, "Restarting in 600 seconds") {
		t.Errorf("stdout = %q", stdout)
	}
}

// TestOneShotJoinsArguments means both `rcon "Restart 600"` and `rcon Restart
// 600` do what they obviously mean.
func TestOneShotJoinsArguments(t *testing.T) {
	var got string
	srv, err := rcontest.New(rcontest.Respond(testPassword, 0, func(command string) string {
		got = command
		return "ok"
	}))
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(srv.Close)

	if code, _, stderr := run(t, "", "-a", srv.Addr(), "-p", testPassword, "Restart", "600"); code != cli.ExitOK {
		t.Fatalf("exit = %d (stderr: %s)", code, stderr)
	}
	if got != "Restart 600" {
		t.Errorf("server received %q, want %q", got, "Restart 600")
	}
}

// TestFailureExitsNonZero is the property the preStop hook depends on: it sleeps
// out a 600 second drain only when the restart was actually accepted, so a
// failure reported as success would hold every terminating pod open.
func TestFailureExitsNonZero(t *testing.T) {
	srv := serve(t, "a-different-password", "unreachable")

	code, _, stderr := run(t, "", "-a", srv.Addr(), "-p", testPassword, "Restart 600")

	if code != cli.ExitFailure {
		t.Fatalf("exit = %d, want %d", code, cli.ExitFailure)
	}
	if !strings.Contains(stderr, "authentication failed") {
		t.Errorf("stderr = %q, want it to name the problem", stderr)
	}
}

// TestUsageErrorsExitTwo keeps a bad invocation distinguishable from a command
// that ran and failed.
func TestUsageErrorsExitTwo(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"address without a port", []string{"-a", "127.0.0.1", "status"}, "host:port"},
		{"unknown flag", []string{"--bogus", "status"}, "unknown flag"},
		{"timeout out of range", []string{"-a", "h:1", "--timeoutSeconds", "0", "status"}, "timeoutSeconds"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := run(t, "", tc.args...)
			if code != cli.ExitUsage {
				t.Fatalf("exit = %d, want %d (stderr: %s)", code, cli.ExitUsage, stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr = %q, want it to mention %q", stderr, tc.want)
			}
		})
	}
}

// TestErrorsAreNotStuttered guards the message format. The library prefixes its
// errors with its package name and so does this command, so without care every
// line reads "rcon: rcon: ...".
func TestErrorsAreNotStuttered(t *testing.T) {
	srv := serve(t, "a-different-password", "unreachable")

	_, _, stderr := run(t, "", "-a", srv.Addr(), "-p", testPassword, "status")

	if strings.Contains(stderr, "rcon: rcon:") { //nolint:dupword // the repetition is what is being detected
		t.Errorf("stderr stutters: %q", stderr)
	}
	if !strings.HasPrefix(stderr, "rcon: ") {
		t.Errorf("stderr = %q, want it to name the program once", stderr)
	}
}

// TestPipedStdinRunsEachLine is what makes the command usable from a script
// without a shell loop.
func TestPipedStdinRunsEachLine(t *testing.T) {
	var commands []string
	srv, err := rcontest.New(rcontest.Respond(testPassword, 0, func(command string) string {
		commands = append(commands, command)
		return "ack " + command
	}))
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(srv.Close)

	code, stdout, stderr := run(t, "status\n\nPlayerInfoAll\n", "-a", srv.Addr(), "-p", testPassword)

	if code != cli.ExitOK {
		t.Fatalf("exit = %d (stderr: %s)", code, stderr)
	}
	if len(commands) != 2 {
		t.Fatalf("server received %v, want the 2 non-blank lines", commands)
	}
	if !strings.Contains(stdout, "ack status") || !strings.Contains(stdout, "ack PlayerInfoAll") {
		t.Errorf("stdout = %q", stdout)
	}
}

// TestPipedStdinStopsOnFailure keeps a scripted run from hiding an error behind
// whatever the later commands did.
func TestPipedStdinStopsOnFailure(t *testing.T) {
	srv := serve(t, "a-different-password", "unreachable")

	code, _, _ := run(t, "first\nsecond\n", "-a", srv.Addr(), "-p", testPassword)

	if code != cli.ExitFailure {
		t.Errorf("exit = %d, want %d", code, cli.ExitFailure)
	}
}

// TestExitOnStdinKeyword lets an interactive session end without a signal.
func TestExitOnStdinKeyword(t *testing.T) {
	srv := serve(t, testPassword, "ok")

	code, _, stderr := run(t, "exit\n", "-a", srv.Addr(), "-p", testPassword)

	if code != cli.ExitOK {
		t.Errorf("exit = %d, want %d (stderr: %s)", code, cli.ExitOK, stderr)
	}
}

// TestPasswordFromEnvironment is the form that keeps the secret off the process
// list, and the one the cluster already has set.
func TestPasswordFromEnvironment(t *testing.T) {
	srv := serve(t, testPassword, "ok")

	t.Chdir(t.TempDir())
	t.Setenv("RCON_PASSWORD", testPassword)
	t.Setenv("RCON_ADDRESS", srv.Addr())

	var out, errOut bytes.Buffer
	code := cli.RunForTest(t.Context(), []string{"status"}, strings.NewReader(""), &out, &errOut)

	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, cli.ExitOK, errOut.String())
	}
	if !strings.Contains(out.String(), "ok") {
		t.Errorf("stdout = %q", out.String())
	}
}

// TestHostAndPortFromEnvironment covers the variables the sibling services
// already use, so one environment configures all of them.
func TestHostAndPortFromEnvironment(t *testing.T) {
	srv := serve(t, testPassword, "ok")
	host, port, _ := strings.Cut(srv.Addr(), ":")

	t.Chdir(t.TempDir())
	t.Setenv("RCON_HOST", host)
	t.Setenv("RCON_PORT", port)
	t.Setenv("RCON_PASSWORD", testPassword)

	var out, errOut bytes.Buffer
	code := cli.RunForTest(t.Context(), []string{"status"}, strings.NewReader(""), &out, &errOut)

	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, cli.ExitOK, errOut.String())
	}
}

// TestFlagBeatsEnvironment pins the precedence an operator relies on when
// overriding a pod's baked-in settings from a shell.
func TestFlagBeatsEnvironment(t *testing.T) {
	srv := serve(t, testPassword, "from the flag")

	t.Chdir(t.TempDir())
	t.Setenv("RCON_ADDRESS", "127.0.0.1:1")
	t.Setenv("RCON_PASSWORD", testPassword)

	var out, errOut bytes.Buffer
	code := cli.RunForTest(t.Context(), []string{"-a", srv.Addr(), "status"}, strings.NewReader(""), &out, &errOut)

	if code != cli.ExitOK {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, cli.ExitOK, errOut.String())
	}
	if !strings.Contains(out.String(), "from the flag") {
		t.Errorf("stdout = %q, want the flag's address to have won", out.String())
	}
}
