package rcon_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/rcon"
	"github.com/USA-RedDragon/rcon/rcontest"
)

func TestNewDefaults(t *testing.T) {
	client := rcon.New("127.0.0.1:27015", "pw")

	if got := client.Addr(); got != "127.0.0.1:27015" {
		t.Errorf("Addr() = %q", got)
	}
	if got := client.Timeout(); got != rcon.DefaultTimeout {
		t.Errorf("Timeout() = %s, want the default %s", got, rcon.DefaultTimeout)
	}
}

// TestWithTimeoutIgnoresNonPositive keeps a client from ending up with no
// deadline at all, which would hang forever against a server that accepts a
// connection and then says nothing.
func TestWithTimeoutIgnoresNonPositive(t *testing.T) {
	for _, timeout := range []time.Duration{0, -time.Second} {
		client := rcon.New("127.0.0.1:27015", "pw", rcon.WithTimeout(timeout))
		if got := client.Timeout(); got != rcon.DefaultTimeout {
			t.Errorf("WithTimeout(%s) left Timeout() = %s, want the default", timeout, got)
		}
	}
}

func TestWithTimeoutApplies(t *testing.T) {
	client := rcon.New("127.0.0.1:27015", "pw", rcon.WithTimeout(3*time.Second))
	if got := client.Timeout(); got != 3*time.Second {
		t.Errorf("Timeout() = %s, want 3s", got)
	}
}

// TestWithMaxConcurrentClampsToOne covers the setting that would otherwise mean
// the opposite of what an operator intended: a zero-capacity limit reports every
// command as busy rather than allowing unlimited ones.
func TestWithMaxConcurrentClampsToOne(t *testing.T) {
	srv, err := rcontest.New(rcontest.Respond("pw", 0, func(string) string {
		return "ok"
	}))
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(srv.Close)

	for _, limit := range []int{0, -5} {
		client := rcon.New(srv.Addr(), "pw", rcon.WithMaxConcurrent(limit))
		if _, err := client.Execute(t.Context(), "ping"); err != nil {
			t.Errorf("WithMaxConcurrent(%d) made every command fail: %v", limit, err)
		}
	}
}

// TestExecuteRejectsOverLongCommandBeforeDialing pins that the check happens
// first. Deferring it would report an over-long command as a connection error
// whenever the server happened to be down, sending the operator after the wrong
// problem.
func TestExecuteRejectsOverLongCommandBeforeDialing(t *testing.T) {
	// Nothing is listening here, so reaching the network at all would surface as
	// a connection error rather than ErrCommandTooLong.
	client := rcon.New("127.0.0.1:1", "pw", rcon.WithTimeout(5*time.Second))

	start := time.Now()
	_, err := client.Execute(t.Context(), strings.Repeat("x", rcon.MaxCommandLen+1))

	if !errors.Is(err, rcon.ErrCommandTooLong) {
		t.Fatalf("error = %v, want ErrCommandTooLong", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %s, so it dialled before checking the length", elapsed)
	}
}

func TestExecuteAcceptsCommandAtTheLimit(t *testing.T) {
	command := strings.Repeat("x", rcon.MaxCommandLen)
	srv, err := rcontest.New(rcontest.Respond("pw", 0, func(got string) string {
		if len(got) != rcon.MaxCommandLen {
			t.Errorf("server received %d bytes, want %d", len(got), rcon.MaxCommandLen)
		}
		return "ok"
	}))
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(srv.Close)

	client := rcon.New(srv.Addr(), "pw")
	if _, err := client.Execute(t.Context(), command); err != nil {
		t.Errorf("a command of exactly MaxCommandLen was rejected: %v", err)
	}
}

// TestExecuteReportsNotRCON separates a wrong port from a wrong password. The
// two look similar from the outside and send an operator to completely different
// places, so the library distinguishes them rather than making the caller guess.
func TestExecuteReportsNotRCON(t *testing.T) {
	srv, err := rcontest.New(func(f *rcontest.Framer) {
		if _, rerr := f.Read(); rerr != nil {
			return
		}
		// Well-formed framing carrying a packet type no RCON server would answer
		// authentication with, which is what a wrong port tends to look like once
		// the bytes happen to parse.
		if werr := f.Write(rcontest.TypeAuth, 1, "who are you"); werr != nil {
			return
		}
		for {
			if _, rerr := f.Read(); rerr != nil {
				return
			}
		}
	})
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(srv.Close)

	client := rcon.New(srv.Addr(), "pw", rcon.WithTimeout(2*time.Second))
	_, execErr := client.Execute(t.Context(), "status")

	if !errors.Is(execErr, rcon.ErrNotRCON) {
		t.Fatalf("error = %v, want ErrNotRCON", execErr)
	}
	if errors.Is(execErr, rcon.ErrAuthFailed) {
		t.Error("a wrong port was reported as a wrong password")
	}
}

// TestAuthResponseWithWrongIDIsProtocolError keeps a desynchronised exchange
// from being blamed on the password.
func TestAuthResponseWithWrongIDIsProtocolError(t *testing.T) {
	srv, err := rcontest.New(func(f *rcontest.Framer) {
		if _, rerr := f.Read(); rerr != nil {
			return
		}
		if werr := f.Write(rcontest.TypeAuthResponse, 99, ""); werr != nil {
			return
		}
		for {
			if _, rerr := f.Read(); rerr != nil {
				return
			}
		}
	})
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(srv.Close)

	client := rcon.New(srv.Addr(), "pw", rcon.WithTimeout(2*time.Second))
	_, execErr := client.Execute(t.Context(), "status")

	if !errors.Is(execErr, rcon.ErrProtocol) {
		t.Fatalf("error = %v, want ErrProtocol", execErr)
	}
	if errors.Is(execErr, rcon.ErrAuthFailed) {
		t.Error("a desynchronised exchange was reported as a wrong password")
	}
}

// TestTimeoutErrorMessageIsStable guards a string that reaches end users through
// at least one consumer's HTTP responses.
func TestTimeoutErrorMessageIsStable(t *testing.T) {
	err := &rcon.TimeoutError{Addr: "example:27015", Timeout: time.Second}

	if got, want := err.Error(), "rcon: example:27015 did not respond within 1s"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}
