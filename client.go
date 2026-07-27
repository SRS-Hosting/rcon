// Package rcon is a Source RCON client.
//
// It exists because the readily available Go clients read exactly one packet per
// command, and a response larger than about 4KB is split across several. That
// truncation is silent: a long response comes back looking like a short one,
// with no error to say otherwise. This client reassembles multi-packet responses
// and, when one is cut short anyway, says so explicitly while still handing back
// what did arrive.
//
// The design assumes the far end is a game server running RCON on its game
// thread, so commands are bounded: one deadline covers a whole exchange, and
// callers past a configured concurrency limit fail fast rather than queue.
//
//	client := rcon.New("127.0.0.1:27015", password)
//	output, err := client.Execute(ctx, "status")
package rcon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"
)

// Defaults applied by New when the corresponding option is not given.
const (
	// DefaultTimeout bounds a whole exchange: connect, authenticate, send, receive.
	DefaultTimeout = 10 * time.Second

	// DefaultMaxConcurrent is deliberately small. Source servers handle RCON on
	// their main thread and cap or ban clients that pile on connections, so this
	// is headroom for a few callers at once, not a throughput setting.
	DefaultMaxConcurrent = 4
)

// MaxCommandLen is the longest command a Source server will accept. Anything
// longer is rejected before dialling, so an over-long command reports itself as
// such instead of as whatever the connection happened to do.
const MaxCommandLen = 1000

// ErrBusy reports that every concurrent-command slot was taken. It is
// backpressure rather than a failure of the RCON server, and worth
// distinguishing: an ErrBusy is worth retrying in a moment, where most other
// errors mean something is actually wrong.
var ErrBusy = errors.New("rcon: too many commands already in flight")

// ErrCommandTooLong reports a command over MaxCommandLen bytes. It is returned
// before any connection is opened.
var ErrCommandTooLong = fmt.Errorf("rcon: command must be at most %d bytes", MaxCommandLen)

// Client executes commands against a Source RCON server.
//
// A fresh connection is opened per command. RCON connections do not survive a
// server restart and there is no state worth keeping warm, so reconnecting each
// time removes a whole class of stale-socket failures for the cost of one TCP
// handshake and auth round trip.
//
// A Client is safe for concurrent use.
type Client struct {
	addr     string
	password string
	timeout  time.Duration
	// slots bounds how many exchanges may be in flight at once, so a handful of
	// concurrent callers cannot become a matching pile of connections against the
	// game server.
	slots chan struct{}
}

// Option configures a Client.
type Option func(*Client)

// WithTimeout sets the deadline covering one complete exchange.
//
// Values of zero or less are ignored, leaving DefaultTimeout in place: a client
// with no deadline at all hangs forever against a server that accepts a
// connection and then says nothing, which is a common way for a game server to
// fail.
func WithTimeout(timeout time.Duration) Option {
	return func(c *Client) {
		if timeout > 0 {
			c.timeout = timeout
		}
	}
}

// WithMaxConcurrent sets how many exchanges may be in flight at once. Callers
// past the limit get ErrBusy rather than being queued: waiting would spend the
// caller's deadline on the queue and then hand it a truncated budget for the
// exchange itself.
//
// Values below 1 are raised to 1, because a zero-capacity limit would report
// ErrBusy for every command rather than meaning "unlimited".
func WithMaxConcurrent(maxConcurrent int) Option {
	return func(c *Client) {
		c.slots = make(chan struct{}, max(maxConcurrent, 1))
	}
}

// New returns a Client for the RCON server at addr, which must be in host:port
// form.
func New(addr, password string, opts ...Option) *Client {
	c := &Client{
		addr:     addr,
		password: password,
		timeout:  DefaultTimeout,
		slots:    make(chan struct{}, DefaultMaxConcurrent),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// Addr returns the address of the RCON server.
func (c *Client) Addr() string { return c.addr }

// Timeout returns the per-command deadline.
func (c *Client) Timeout() time.Duration { return c.timeout }

// TimeoutError reports that an exchange did not finish within the deadline.
// Callers use errors.As to tell a slow or dead server apart from a server that
// answered with a failure:
//
//	var timeout *rcon.TimeoutError
//	if errors.As(err, &timeout) { ... }
//
// It deliberately does not implement net.Error's Timeout() bool: that method
// name would collide with the Timeout field, and the field is the more useful of
// the two since it tells a caller what budget was actually exceeded.
type TimeoutError struct {
	Addr    string
	Timeout time.Duration
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("rcon: %s did not respond within %s", e.Addr, e.Timeout)
}

type result struct {
	body string
	err  error
}

// Execute runs command on the RCON server and returns its response.
//
// The entire exchange, connect, authenticate, send, receive, shares a single
// deadline, the smaller of ctx and the configured timeout. The socket deadline
// derived from that is the primary bound; the select below is what stops the
// caller waiting past it. The connection watches ctx itself from the moment it
// is dialled (see dialAndAuth) and closes on ctx.Done, so a goroutine
// abandoned here, whether to the deadline or to an early cancellation, has its
// blocked read fail immediately and releases its slot and socket promptly,
// rather than holding both against the game server for up to another full
// timeout after the caller already gave up.
//
// A response cut short still returns the part that arrived, paired with
// ErrTruncated. Every other error returns an empty body.
func (c *Client) Execute(ctx context.Context, command string) (string, error) {
	// Checked before opening a connection, so an over-long command cannot be
	// misreported as a connection problem when the server happens to be down.
	if len(command) > MaxCommandLen {
		return "", ErrCommandTooLong
	}

	// Claimed before any work starts, and released by the goroutine below rather
	// than on return from here, so the count tracks live upstream connections
	// rather than callers still waiting on one.
	select {
	case c.slots <- struct{}{}:
	default:
		return "", ErrBusy
	}

	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	// Buffered so the goroutine can always deliver and exit, even once we have
	// stopped listening.
	ch := make(chan result, 1)

	go func() {
		defer func() { <-c.slots }()
		body, err := c.execute(ctx, command)
		ch <- result{body: body, err: err}
	}()

	select {
	case r := <-ch:
		return r.body, r.err
	case <-ctx.Done():
		// Nothing to clean up here: the goroutine's connection closes itself on
		// this same ctx.Done and the goroutine exits on its own shortly after.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", c.timedOut()
		}
		return "", fmt.Errorf("rcon: %s: %w", c.addr, ctx.Err())
	}
}

// execute performs the blocking exchange. It is only ever called from the
// goroutine started by Execute, and needs no supervision from it: dialAndAuth
// arranges for the connection to close itself when ctx is done, which is what
// unblocks the reads below once the caller has given up.
func (c *Client) execute(ctx context.Context, command string) (string, error) {
	conn, err := dialAndAuth(ctx, c.addr, c.password, c.timeout)
	if err != nil {
		// net.ErrClosed means the connection's own ctx watcher shut the socket
		// mid-auth, so it stands for the deadline or cancellation that fired it
		// and is reported the same way as the deadline itself.
		if isTimeout(err) || errors.Is(err, net.ErrClosed) {
			return "", c.timedOut()
		}
		return "", err
	}
	defer func() {
		if cerr := conn.Close(); cerr != nil && !errors.Is(cerr, net.ErrClosed) {
			slog.Debug("close rcon connection", "addr", c.addr, "error", cerr)
		}
	}()

	body, err := conn.execute(command)
	switch {
	case errors.Is(err, ErrTruncated):
		// Deliberately passed through with its body: a partial response is still
		// worth having, and the caller is better placed than this package to judge
		// whether it is usable.
		return body, err
	case err != nil:
		if isTimeout(err) || errors.Is(err, net.ErrClosed) {
			return "", c.timedOut()
		}
		return "", fmt.Errorf("rcon: execute %q: %w", command, err)
	}
	return body, nil
}

func (c *Client) timedOut() *TimeoutError {
	return &TimeoutError{Addr: c.addr, Timeout: c.timeout}
}

// isTimeout reports whether err is a socket deadline firing. The socket deadline
// can trip just before the context does, and the two mean the same thing to a
// caller, so they are reported the same way.
func isTimeout(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
