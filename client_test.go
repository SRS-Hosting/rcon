package rcon_test

import (
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/USA-RedDragon/rcon"
	"github.com/USA-RedDragon/rcon/rcontest"
)

const testPassword = "swordfish"

// stall blocks a handler until its connection goes away.
//
// A handler that simply blocks forever would keep the fake server's Close
// waiting on it, so tests that need an unresponsive server wait on the socket
// instead: reading fails as soon as the connection is closed, whether by the
// client giving up or by the server shutting down.
func stall(f *rcontest.Framer) {
	for {
		if _, err := f.Read(); err != nil {
			return
		}
	}
}

// serve starts a fake server with handler and returns a client pointed at it.
func serve(t *testing.T, handler rcontest.Handler, timeout time.Duration) *rcon.Client {
	t.Helper()
	srv, err := rcontest.New(handler)
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(srv.Close)
	return rcon.New(srv.Addr(), testPassword, rcon.WithTimeout(timeout))
}

func TestExecuteSinglePacket(t *testing.T) {
	client := serve(t, rcontest.Respond(testPassword, 0, func(string) string {
		return "(PlayerInfoAll): Total Players: 0."
	}), 2*time.Second)

	body, err := client.Execute(t.Context(), "PlayerInfoAll")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if body != "(PlayerInfoAll): Total Players: 0." {
		t.Errorf("body = %q", body)
	}
}

// TestExecuteReassemblesMultiplePackets is the reason this package does not use
// an off-the-shelf client: a full player list exceeds one packet, and reading
// only the first would report a busy server as a quiet one.
func TestExecuteReassemblesMultiplePackets(t *testing.T) {
	// Long enough to need several packets, and built from lines so that a
	// reassembly that inserts or drops bytes at a chunk boundary shows up.
	var want strings.Builder
	for i := range 400 {
		want.WriteString("Name: player")
		want.WriteString(strings.Repeat("x", i%7))
		want.WriteString(" / AGID: 746-132-258 / Growth: 0.5\n")
	}

	client := serve(t, rcontest.Respond(testPassword, 4000, func(string) string {
		return want.String()
	}), 5*time.Second)

	body, err := client.Execute(t.Context(), "PlayerInfoAll")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if body != want.String() {
		t.Errorf("reassembled body differs: got %d bytes, want %d", len(body), want.Len())
	}
}

// TestExecuteToleratesEmptyPacketBeforeAuthResponse covers servers that precede
// the auth verdict with an empty value packet.
func TestExecuteToleratesEmptyPacketBeforeAuthResponse(t *testing.T) {
	client := serve(t, func(f *rcontest.Framer) {
		p, err := f.Read()
		if err != nil {
			return
		}
		if err := f.Write(rcontest.TypeResponseValue, p.ID, ""); err != nil {
			return
		}
		if err := f.Write(rcontest.TypeAuthResponse, p.ID, ""); err != nil {
			return
		}
		for {
			cmd, err := f.Read()
			if err != nil {
				return
			}
			if err := f.Write(rcontest.TypeResponseValue, cmd.ID, "pong"); err != nil {
				return
			}
		}
	}, 2*time.Second)

	body, err := client.Execute(t.Context(), "ping")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if body != "pong" {
		t.Errorf("body = %q, want %q", body, "pong")
	}
}

func TestExecuteRejectsBadPassword(t *testing.T) {
	client := serve(t, rcontest.Respond("a-different-password", 0, func(string) string {
		return "unreachable"
	}), 2*time.Second)

	_, err := client.Execute(t.Context(), "PlayerInfoAll")
	if !errors.Is(err, rcon.ErrAuthFailed) {
		t.Fatalf("Execute error = %v, want ErrAuthFailed", err)
	}
}

// TestExecuteReturnsPartialBodyOnTruncation is the degraded case that matters:
// a server that stops answering mid-response must yield what did arrive, so the
// count it reported can be compared against the records actually received.
func TestExecuteReturnsPartialBodyOnTruncation(t *testing.T) {
	client := serve(t, func(f *rcontest.Framer) {
		p, err := f.Read()
		if err != nil {
			return
		}
		if err := f.Write(rcontest.TypeAuthResponse, p.ID, ""); err != nil {
			return
		}
		cmd, err := f.Read()
		if err != nil {
			return
		}
		if err := f.Write(rcontest.TypeResponseValue, cmd.ID, "first chunk "); err != nil {
			return
		}
		if err := f.Write(rcontest.TypeResponseValue, cmd.ID, "second chunk"); err != nil {
			return
		}
		// Then never send the marker, so the client's deadline is what ends it.
		stall(f)
	}, 300*time.Millisecond)

	body, err := client.Execute(t.Context(), "PlayerInfoAll")
	if !errors.Is(err, rcon.ErrTruncated) {
		t.Fatalf("Execute error = %v, want ErrTruncated", err)
	}
	if body != "first chunk second chunk" {
		t.Errorf("partial body = %q, want the chunks that did arrive", body)
	}
}

// TestExecuteRejectsOversizedPacket guards the allocation the size field would
// otherwise dictate: it arrives before anything that could corroborate it.
func TestExecuteRejectsOversizedPacket(t *testing.T) {
	client := serve(t, func(f *rcontest.Framer) {
		p, err := f.Read()
		if err != nil {
			return
		}
		if err := f.Write(rcontest.TypeAuthResponse, p.ID, ""); err != nil {
			return
		}
		if _, err := f.Read(); err != nil {
			return
		}
		header := binary.LittleEndian.AppendUint32(nil, 1<<30)
		if err := f.WriteRaw(header); err != nil {
			return
		}
		stall(f)
	}, 2*time.Second)

	done := make(chan error, 1)
	go func() {
		_, err := client.Execute(t.Context(), "PlayerInfoAll")
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, rcon.ErrProtocol) {
			t.Fatalf("Execute error = %v, want ErrProtocol", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Execute did not reject an implausible packet size promptly")
	}
}

// TestExecuteToleratesShortPadding covers a server that terminates a body with
// one NUL instead of two. It is cosmetic, and failing the exchange over it would
// turn a quirk into an outage.
func TestExecuteToleratesShortPadding(t *testing.T) {
	client := serve(t, func(f *rcontest.Framer) {
		p, err := f.Read()
		if err != nil {
			return
		}
		if err := f.Write(rcontest.TypeAuthResponse, p.ID, ""); err != nil {
			return
		}
		cmd, err := f.Read()
		if err != nil {
			return
		}
		body := "short padding"
		// Size counts one terminator instead of the two the spec calls for.
		raw := binary.LittleEndian.AppendUint32(nil, uint32(9+len(body))) //nolint:gosec // fixed-size test fixture
		raw = binary.LittleEndian.AppendUint32(raw, uint32(cmd.ID))       //nolint:gosec // signed on the wire
		raw = binary.LittleEndian.AppendUint32(raw, uint32(rcontest.TypeResponseValue))
		raw = append(raw, body...)
		raw = append(raw, 0)
		if err := f.WriteRaw(raw); err != nil {
			return
		}
		for {
			marker, err := f.Read()
			if err != nil {
				return
			}
			if err := f.Write(rcontest.TypeResponseValue, marker.ID, ""); err != nil {
				return
			}
		}
	}, 2*time.Second)

	body, err := client.Execute(t.Context(), "PlayerInfoAll")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if body != "short padding" {
		t.Errorf("body = %q", body)
	}
}

func TestExecuteReportsTimeoutForDeadServer(t *testing.T) {
	client := serve(t, stall, 200*time.Millisecond)

	_, err := client.Execute(t.Context(), "PlayerInfoAll")
	var timeoutErr *rcon.TimeoutError
	if !errors.As(err, &timeoutErr) {
		t.Fatalf("Execute error = %v, want *TimeoutError", err)
	}
}

// TestExecuteRejectsExcessConcurrency covers the backpressure contract: callers
// past the limit fail fast instead of queueing, because queueing would spend
// their deadline waiting and then hand them what is left of it.
func TestExecuteRejectsExcessConcurrency(t *testing.T) {
	release := make(chan struct{})
	srv, err := rcontest.New(func(f *rcontest.Framer) {
		p, rerr := f.Read()
		if rerr != nil {
			return
		}
		if werr := f.Write(rcontest.TypeAuthResponse, p.ID, ""); werr != nil {
			return
		}
		<-release
	})
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(srv.Close)
	defer close(release)

	client := rcon.New(srv.Addr(), testPassword,
		rcon.WithTimeout(2*time.Second), rcon.WithMaxConcurrent(1))

	busy := make(chan error, 1)
	go func() {
		_, execErr := client.Execute(t.Context(), "PlayerInfoAll")
		busy <- execErr
	}()

	// Wait for the single slot to be taken before competing for it.
	deadline := time.Now().Add(time.Second)
	for srv.Connections() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	if _, err := client.Execute(t.Context(), "PlayerInfoAll"); !errors.Is(err, rcon.ErrBusy) {
		t.Fatalf("second Execute error = %v, want ErrBusy", err)
	}
}

// TestExecuteErrorsCarryTheLibraryPrefix pins the contract that every error
// leaving this package starts with "rcon: ", so consumers can attribute a
// failure to the library without re-prefixing and stuttering. The dial failure
// exercised here was one of the paths that used to escape without it.
func TestExecuteErrorsCarryTheLibraryPrefix(t *testing.T) {
	// Nothing listens on port 1, so the exchange fails at the connect step.
	client := rcon.New("127.0.0.1:1", testPassword, rcon.WithTimeout(500*time.Millisecond))

	_, err := client.Execute(t.Context(), "status")
	if err == nil {
		t.Fatal("Execute against a dead port returned no error")
	}
	if !strings.HasPrefix(err.Error(), "rcon: ") {
		t.Errorf("error = %q, want it to start with the library prefix", err)
	}
}

func TestExecuteHonoursCallerCancellation(t *testing.T) {
	client := serve(t, stall, time.Minute)

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	if _, err := client.Execute(ctx, "PlayerInfoAll"); err == nil {
		t.Fatal("Execute returned no error after cancellation")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("Execute waited %s, ignoring cancellation", elapsed)
	}
}
