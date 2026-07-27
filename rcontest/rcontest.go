// Package rcontest provides a scriptable Source RCON server for tests.
//
// The framing is implemented independently of the client's, so a passing test is
// evidence that two separate readings of the wire format agree, rather than
// evidence that one implementation is self-consistent.
package rcontest

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
)

// Packet types, named from the client's point of view. Type 2 is both
// SERVERDATA_EXECCOMMAND inbound and SERVERDATA_AUTH_RESPONSE outbound.
const (
	TypeResponseValue int32 = 0
	TypeExecCommand   int32 = 2
	TypeAuthResponse  int32 = 2
	TypeAuth          int32 = 3
)

// AuthFailedID is the id a server sends to reject a password.
const AuthFailedID int32 = -1

// maxBodyLen is the protocol's packet ceiling less the fixed overhead. Real
// servers split anything longer across packets, which is what Respond does.
const maxBodyLen = 4096 - 10

// Packet is one RCON packet.
type Packet struct {
	ID   int32
	Type int32
	Body string
}

// Framer reads and writes packets on one connection.
type Framer struct {
	conn net.Conn
	br   *bufio.Reader
}

// Conn exposes the underlying connection for tests that need to close or stall
// it directly.
func (f *Framer) Conn() net.Conn { return f.conn }

// Read reads one packet.
func (f *Framer) Read() (Packet, error) {
	var header [4]byte
	if _, err := io.ReadFull(f.br, header[:]); err != nil {
		return Packet{}, err
	}
	size := binary.LittleEndian.Uint32(header[:])
	if size < 10 || size > 1<<20 {
		return Packet{}, fmt.Errorf("rcontest: implausible packet size %d", size)
	}
	rest := make([]byte, size)
	if _, err := io.ReadFull(f.br, rest); err != nil {
		return Packet{}, err
	}
	// Ids and types are signed on the wire, so these are reinterpretations
	// rather than narrowing conversions.
	return Packet{
		ID:   int32(binary.LittleEndian.Uint32(rest[0:4])), //nolint:gosec // signed on the wire
		Type: int32(binary.LittleEndian.Uint32(rest[4:8])), //nolint:gosec // signed on the wire
		Body: strings.TrimRight(string(rest[8:]), "\x00"),
	}, nil
}

// Write frames and sends one packet.
func (f *Framer) Write(typ, id int32, body string) error {
	if len(body) > maxBodyLen {
		return fmt.Errorf("rcontest: body of %d bytes exceeds the %d byte limit", len(body), maxBodyLen)
	}
	buf := make([]byte, 0, 14+len(body))
	buf = binary.LittleEndian.AppendUint32(buf, uint32(10+len(body))) //nolint:gosec // bounded above
	buf = binary.LittleEndian.AppendUint32(buf, uint32(id))           //nolint:gosec // signed on the wire
	buf = binary.LittleEndian.AppendUint32(buf, uint32(typ))          //nolint:gosec // signed on the wire
	buf = append(buf, body...)
	buf = append(buf, 0, 0)
	return f.WriteRaw(buf)
}

// WriteRaw sends bytes verbatim, for tests that need a frame the framer would
// never produce.
func (f *Framer) WriteRaw(b []byte) error {
	if _, err := f.conn.Write(b); err != nil {
		return fmt.Errorf("rcontest: write: %w", err)
	}
	return nil
}

// Handler answers one accepted connection, including authentication.
type Handler func(f *Framer)

// Server is a fake RCON server listening on loopback.
type Server struct {
	ln net.Listener

	wg    sync.WaitGroup
	conns atomic.Int64

	mu     sync.Mutex
	open   []net.Conn
	closed bool
}

// New starts a server that passes each accepted connection to handler.
func New(handler Handler) (*Server, error) {
	var lc net.ListenConfig
	ln, err := lc.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("rcontest: listen: %w", err)
	}
	s := &Server{ln: ln}
	s.wg.Add(1)
	go s.serve(handler)
	return s, nil
}

// Addr returns the host:port to point a client at.
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Connections reports how many connections have been accepted. It is what a
// caching test asserts on: a cache that works means the second scrape adds none.
func (s *Server) Connections() int { return int(s.conns.Load()) }

// Close stops the server and waits for its handlers.
//
// Open connections are closed first, so a handler deliberately stalling to
// trigger a client-side deadline cannot hold the test open.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	open := s.open
	s.open = nil
	s.mu.Unlock()

	s.ln.Close()
	for _, c := range open {
		c.Close()
	}
	s.wg.Wait()
}

func (s *Server) serve(handler Handler) {
	defer s.wg.Done()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.conns.Add(1)

		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			conn.Close()
			return
		}
		s.open = append(s.open, conn)
		s.mu.Unlock()

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer conn.Close()
			handler(&Framer{conn: conn, br: bufio.NewReader(conn)})
		}()
	}
}

// Respond returns a Handler that authenticates against password and then
// answers commands the way a real server does: a non-empty command gets
// fn(command) split into chunks of at most chunkSize bytes, and the empty
// command the client uses as an end-of-response marker gets the junk body a real
// server sends back. A chunkSize of zero sends each response in one packet.
func Respond(password string, chunkSize int, fn func(command string) string) Handler {
	return func(f *Framer) {
		if !authenticate(f, password) {
			return
		}
		for {
			p, err := f.Read()
			if err != nil {
				return
			}
			if p.Type != TypeExecCommand {
				return
			}
			if p.Body == "" {
				// The marker packet. Real servers answer it with a fixed scrap of
				// binary, so reproduce that rather than an empty body: the client
				// must discard it either way.
				if err := f.Write(TypeResponseValue, p.ID, "\x00\x01\x00\x00"); err != nil {
					return
				}
				continue
			}
			for _, chunk := range split(fn(p.Body), chunkSize) {
				if err := f.Write(TypeResponseValue, p.ID, chunk); err != nil {
					return
				}
			}
		}
	}
}

// authenticate performs the standard handshake and reports whether it succeeded.
func authenticate(f *Framer, password string) bool {
	p, err := f.Read()
	if err != nil || p.Type != TypeAuth {
		return false
	}
	if p.Body != password {
		_ = f.Write(TypeAuthResponse, AuthFailedID, "")
		return false
	}
	return f.Write(TypeAuthResponse, p.ID, "") == nil
}

// split cuts s into chunks of at most n bytes, deliberately without regard for
// line boundaries: a real multi-packet response splits mid-line, and a client
// that only works when chunks align to newlines is broken.
func split(s string, n int) []string {
	if n <= 0 || len(s) <= n {
		return []string{s}
	}
	chunks := make([]string, 0, (len(s)+n-1)/n)
	for len(s) > n {
		chunks = append(chunks, s[:n])
		s = s[n:]
	}
	return append(chunks, s)
}
