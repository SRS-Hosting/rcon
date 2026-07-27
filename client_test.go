package rcon_test

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
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

// TestExecuteReportsAuthHangupAsRejection covers servers that reject a password
// by closing the connection instead of answering with id -1. Surfaced as a bare
// EOF, that sends an operator hunting for a network problem; reported as
// ErrAuthFailed, with the message hedged since the wire proves nothing, the
// first thing checked is the password, which is almost always the fix.
func TestExecuteReportsAuthHangupAsRejection(t *testing.T) {
	client := serve(t, func(f *rcontest.Framer) {
		// Read the auth packet and return without answering; the server closes
		// the connection behind the handler, exactly as these servers do.
		_, _ = f.Read()
	}, 2*time.Second)

	_, err := client.Execute(t.Context(), "PlayerInfoAll")
	if !errors.Is(err, rcon.ErrAuthFailed) {
		t.Fatalf("Execute error = %v, want ErrAuthFailed", err)
	}
	if !strings.Contains(err.Error(), "closed the connection") {
		t.Errorf("error %q does not mention the connection being closed", err)
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

// TestExecuteFreesSlotPromptlyOnCancellation guards against a slot leak: a
// caller that cancelled while the server stalled the auth exchange used to
// leave the goroutine blocked in the auth read until the socket deadline
// fired, holding its slot and socket for nearly the full configured timeout.
// With one slot, a single cancelled call turned every call after it into
// ErrBusy for that whole window. The connection now closes itself on ctx.Done,
// so the slot must come free shortly after the cancellation. The two second
// bound is generous headroom over the instant it should take, while still well
// short of when the five second timeout would have released it.
func TestExecuteFreesSlotPromptlyOnCancellation(t *testing.T) {
	srv, err := rcontest.New(stall)
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(srv.Close)

	client := rcon.New(srv.Addr(), testPassword,
		rcon.WithTimeout(5*time.Second), rcon.WithMaxConcurrent(1))

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if _, err := client.Execute(ctx, "PlayerInfoAll"); err == nil {
		t.Fatal("Execute returned no error after cancellation")
	}

	// Any outcome other than ErrBusy means the slot came free: the poll itself
	// then just times out its short budget against the still-stalled server.
	deadline := time.Now().Add(2 * time.Second)
	for {
		pollCtx, pollCancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		_, err := client.Execute(pollCtx, "PlayerInfoAll")
		pollCancel()
		if !errors.Is(err, rcon.ErrBusy) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("slot still held 2s after cancellation")
		}
		time.Sleep(10 * time.Millisecond)
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

// pagedHandler emulates the way Path of Titans splits a long response: it caps
// each page at pageSize, prefixes the page marker, and serves later pages only
// when asked for by key and index.
func pagedHandler(password, full string, pageSize int) rcontest.Handler {
	const key = 7
	pages := []string{}
	for rest := full; len(rest) > 0; {
		n := min(pageSize, len(rest))
		pages = append(pages, rest[:n])
		rest = rest[n:]
	}

	return rcontest.Respond(password, 0, func(command string) string {
		index := 1
		if after, ok := strings.CutPrefix(command, fmt.Sprintf("Page:%d-", key)); ok {
			parsed, err := strconv.Atoi(after)
			if err != nil || parsed < 1 || parsed > len(pages) {
				return "That page does not exist."
			}
			index = parsed
		}
		return fmt.Sprintf("[Page(Key %d) %d/%d]\n%s", key, index, len(pages), pages[index-1])
	})
}

// TestExecuteFollowsGamePagination covers the second way a response can be
// split. Packet reassembly is not enough on its own: Path of Titans caps a
// response at 4000 characters and pages the rest, so a client that stopped at
// page one would see a fraction of a busy server and have no way to tell.
func TestExecuteFollowsGamePagination(t *testing.T) {
	var full strings.Builder
	for i := range 300 {
		fmt.Fprintf(&full, "entry-%03d-padding-to-make-this-long\n", i)
	}

	client := serve(t, pagedHandler(testPassword, full.String(), 4000), 5*time.Second)

	body, err := client.Execute(t.Context(), "ListQuests")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if body != full.String() {
		t.Errorf("reassembled %d bytes, want %d", len(body), full.Len())
	}
	// The markers are the client's business, not the caller's.
	if strings.Contains(body, "[Page(Key") {
		t.Errorf("page markers leaked into the response:\n%s", body[:200])
	}
}

// TestExecuteSinglePageHasNoMarker keeps the common short response untouched.
func TestExecuteSinglePageHasNoMarker(t *testing.T) {
	client := serve(t, pagedHandler(testPassword, "Total Players: 0.", 4000), 2*time.Second)

	body, err := client.Execute(t.Context(), "PlayerInfoAll")
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if body != "Total Players: 0." {
		t.Errorf("body = %q", body)
	}
}

// TestExecuteReportsExpiredPagesAsTruncation covers a page expiring before it
// is asked for: the game holds pages only for PageTimeout and answers an
// expired page with prose instead of a page. The prose must not be spliced
// into the body as if it were data, and what did arrive must come back flagged
// with ErrTruncated rather than passed off as the whole response.
func TestExecuteReportsExpiredPagesAsTruncation(t *testing.T) {
	client := serve(t, rcontest.Respond(testPassword, 0, func(command string) string {
		if strings.HasPrefix(command, "Page:") {
			return "That page does not exist."
		}
		return "[Page(Key 7) 1/3]\nfirst page only"
	}), 2*time.Second)

	body, err := client.Execute(t.Context(), "ListQuests")
	if !errors.Is(err, rcon.ErrTruncated) {
		t.Fatalf("Execute error = %v, want ErrTruncated", err)
	}
	if body != "first page only" {
		t.Errorf("body = %q, want the page that did arrive with its marker stripped", body)
	}
}

// TestExecutePageFollowingIsBounded guards the round-trip budget: every page is
// a round trip against the game thread, so a server claiming an enormous page
// count with next to nothing in each page must be cut off by the client rather
// than paid one round trip per claimed page until the deadline.
func TestExecutePageFollowingIsBounded(t *testing.T) {
	var requests atomic.Int64
	client := serve(t, rcontest.Respond(testPassword, 0, func(command string) string {
		requests.Add(1)
		if after, ok := strings.CutPrefix(command, "Page:7-"); ok {
			return fmt.Sprintf("[Page(Key 7) %s/100000]\nx", after)
		}
		return "[Page(Key 7) 1/100000]\nx"
	}), 5*time.Second)

	_, err := client.Execute(t.Context(), "ListQuests")
	if !errors.Is(err, rcon.ErrTruncated) {
		t.Fatalf("Execute error = %v, want ErrTruncated", err)
	}
	if n := requests.Load(); n > 300 {
		t.Errorf("server was asked %d times, want the follow loop bounded", n)
	}
}

// TestExecuteRejectsMidSequenceFirstPage keeps a desynchronised exchange from
// being presented as a complete response: a first reply marked as a later page
// means earlier pages exist that were never seen.
func TestExecuteRejectsMidSequenceFirstPage(t *testing.T) {
	client := serve(t, rcontest.Respond(testPassword, 0, func(string) string {
		return "[Page(Key 7) 3/5]\nlate fragment"
	}), 2*time.Second)

	body, err := client.Execute(t.Context(), "ListQuests")
	if !errors.Is(err, rcon.ErrTruncated) {
		t.Fatalf("Execute error = %v, want ErrTruncated", err)
	}
	if body != "late fragment" {
		t.Errorf("body = %q, want the fragment with its marker stripped", body)
	}
}

// TestExecuteStopsWhenPageKeyChanges pins the key check: a page carrying some
// other exchange's key is not part of this response, and splicing it in would
// corrupt the body with no error to say so.
func TestExecuteStopsWhenPageKeyChanges(t *testing.T) {
	client := serve(t, rcontest.Respond(testPassword, 0, func(command string) string {
		if strings.HasPrefix(command, "Page:") {
			return "[Page(Key 9) 2/2]\nsomebody else's page"
		}
		return "[Page(Key 7) 1/2]\nfirst"
	}), 2*time.Second)

	body, err := client.Execute(t.Context(), "ListQuests")
	if !errors.Is(err, rcon.ErrTruncated) {
		t.Fatalf("Execute error = %v, want ErrTruncated", err)
	}
	if body != "first" {
		t.Errorf("body = %q, want only the page that belonged to this response", body)
	}
}

// TestExecuteStripsMarkerFromTruncatedFirstPage keeps the page marker out of a
// partial body: the marker is the client's business even when the response is
// cut short before its sentinel.
func TestExecuteStripsMarkerFromTruncatedFirstPage(t *testing.T) {
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
		if err := f.Write(rcontest.TypeResponseValue, cmd.ID, "[Page(Key 7) 1/9]\npartial "); err != nil {
			return
		}
		if err := f.Write(rcontest.TypeResponseValue, cmd.ID, "data"); err != nil {
			return
		}
		// Then never send the marker, so the client's deadline is what ends it.
		stall(f)
	}, 300*time.Millisecond)

	body, err := client.Execute(t.Context(), "ListQuests")
	if !errors.Is(err, rcon.ErrTruncated) {
		t.Fatalf("Execute error = %v, want ErrTruncated", err)
	}
	if body != "partial data" {
		t.Errorf("body = %q, want the partial body without its marker", body)
	}
}
