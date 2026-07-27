package rcon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"
)

// Packet ids we choose for each stage of an exchange.
//
// execID and sentinelID are the whole reassembly mechanism: a response larger
// than one packet arrives as several, with nothing in any of them saying which
// is last. So the command goes out as execID and is followed immediately by an
// empty command as sentinelID. The server answers in order, so the reply
// carrying sentinelID is proof that every execID packet has already arrived.
const (
	authID     int32 = 1
	execID     int32 = 2
	sentinelID int32 = 3
)

// authFailedID is the id a server returns to reject a password.
const authFailedID int32 = -1

// maxResponseBytes caps one reassembled response. A hundred players produce
// well under 100KB, so this only bounds a server that never sends the sentinel:
// the deadline would catch that too, but not before the body grew without limit.
const maxResponseBytes = 1 << 20

// maxAuthPackets bounds how many packets we will read waiting for an auth
// verdict. Some servers send an empty SERVERDATA_RESPONSE_VALUE first, so more
// than one is normal, but an endless stream of them is not.
const maxAuthPackets = 8

// ErrAuthFailed reports that the server rejected the password. Retrying cannot
// fix it, so callers should surface it rather than treat it as a transient.
var ErrAuthFailed = errors.New("rcon: authentication failed, check the password")

// ErrNotRCON reports that something is listening and answering, but not with
// RCON. It is almost always a wrong port, and it is kept separate from
// ErrAuthFailed because the two send an operator to entirely different places.
var ErrNotRCON = errors.New("rcon: the address is not an RCON server")

// ErrTruncated reports that a response was cut short: the sentinel packet never
// arrived before the deadline, or the response outgrew maxResponseBytes.
//
// It is returned alongside the bytes that did arrive, because a partial response
// is still worth having. What must not happen is a truncated response being read
// as a complete one, so callers that can cross-check the result, against a count
// the server itself reported, for instance, should do so.
var ErrTruncated = errors.New("rcon: response truncated before the end-of-response marker")

// conn is one authenticated RCON connection.
type conn struct {
	net net.Conn
	br  *bufio.Reader
}

// dialAndAuth opens a connection to addr and authenticates it.
//
// The connection gets a single deadline covering everything that follows, taken
// from ctx when it has one. A per-operation deadline would be re-armed at each
// step, so a server that answers every step just inside it could outlast the
// caller's budget in aggregate; one absolute deadline cannot be stretched that
// way.
func dialAndAuth(ctx context.Context, addr, password string, timeout time.Duration) (*conn, error) {
	var dialer net.Dialer
	netConn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("rcon: connect to %s: %w", addr, err)
	}

	if err := netConn.SetDeadline(socketDeadline(ctx, timeout)); err != nil {
		netConn.Close()
		return nil, fmt.Errorf("rcon: set deadline on %s: %w", addr, err)
	}

	c := &conn{net: netConn, br: bufio.NewReader(netConn)}
	if err := c.authenticate(password); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// Close releases the connection. It is safe to call concurrently with a blocked
// read, which is how Execute abandons a timed-out exchange.
func (c *conn) Close() error {
	return c.net.Close()
}

// Bounds on how much of the caller's budget is reserved for reporting.
const (
	minReportingMargin = 50 * time.Millisecond
	maxReportingMargin = time.Second
)

// socketDeadline returns when the socket should give up, deliberately a little
// before the caller's own deadline.
//
// Without that margin the two fire together and the caller's context wins the
// race, so a response that stalled halfway is reported as a bare timeout and the
// bytes that did arrive are discarded. The margin is what lets a truncated
// response come back as ErrTruncated carrying its partial body, which a caller
// can still make use of, instead of as an indistinguishable dead server.
func socketDeadline(ctx context.Context, timeout time.Duration) time.Time {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(timeout)
	}

	budget := time.Until(deadline)
	margin := min(max(budget/10, minReportingMargin), maxReportingMargin)
	if margin >= budget {
		// Too little budget left to reserve any of it; the caller's context is
		// then the only bound, which is better than a deadline already past.
		return deadline
	}
	return deadline.Add(-margin)
}

func (c *conn) authenticate(password string) error {
	if err := writePacket(c.net, typeAuth, authID, password); err != nil {
		return err
	}

	for range maxAuthPackets {
		p, err := readPacket(c.br)
		if err != nil {
			return fmt.Errorf("rcon: read auth response: %w", err)
		}

		switch {
		case p.typ == typeAuthResponse && p.id == authFailedID:
			return ErrAuthFailed
		case p.typ == typeAuthResponse && p.id == authID:
			return nil
		case p.typ == typeAuthResponse:
			// Right packet type, wrong id: the exchange has desynchronised, which
			// is not the same as being told the password is wrong.
			return fmt.Errorf("%w: auth response carried id %d, expected %d",
				ErrProtocol, p.id, authID)
		case p.typ == typeResponseValue:
			// Documented quirk: some servers precede the verdict with an empty
			// value packet. It carries no information, so read past it.
			continue
		default:
			// Well-formed framing, but nothing an RCON server would send here.
			// Overwhelmingly this is a port pointed at some other service.
			return fmt.Errorf("%w: answered authentication with packet type %d", ErrNotRCON, p.typ)
		}
	}
	return fmt.Errorf("%w: no authentication verdict after %d packets", ErrProtocol, maxAuthPackets)
}

// execute runs command and returns the reassembled response.
//
// On truncation it returns what arrived along with ErrTruncated rather than
// discarding it.
func (c *conn) execute(command string) (string, error) {
	if err := writePacket(c.net, typeExecCommand, execID, command); err != nil {
		return "", err
	}
	// Queued immediately behind the real command so the server has both before it
	// starts answering either.
	if err := writePacket(c.net, typeExecCommand, sentinelID, ""); err != nil {
		return "", err
	}

	var body strings.Builder
	for {
		p, err := readPacket(c.br)
		if err != nil {
			// A deadline or a closed connection after some of the response landed
			// is exactly the partial case worth reporting with its data.
			if body.Len() > 0 && isIncomplete(err) {
				return body.String(), ErrTruncated
			}
			return "", fmt.Errorf("read response to %q: %w", command, err)
		}

		if p.id == sentinelID {
			// The server answers the empty command with a junk body of its own,
			// which is not part of the response.
			return body.String(), nil
		}
		if p.typ != typeResponseValue || p.id != execID {
			slog.Debug("ignoring unexpected rcon packet", "type", p.typ, "id", p.id, "bytes", len(p.body))
			continue
		}

		// Chunks split mid-line, so they are joined byte for byte with nothing
		// inserted between them.
		body.WriteString(p.body)
		if body.Len() > maxResponseBytes {
			return body.String(), ErrTruncated
		}
	}
}

// isIncomplete reports whether err ended the response early rather than
// signalling a broken one: a deadline, a peer that hung up, or a socket closed
// out from under us by a timing-out caller.
func isIncomplete(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
