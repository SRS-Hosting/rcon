package rcon

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Source RCON packet types.
//
// Note that 2 means two different things depending on direction: a client sends
// SERVERDATA_EXECCOMMAND and a server answers SERVERDATA_AUTH_RESPONSE, both
// numbered 2. Nothing disambiguates them on the wire, so the reader has to know
// which stage of the exchange it is in.
const (
	typeResponseValue int32 = 0
	typeExecCommand   int32 = 2
	typeAuthResponse  int32 = 2
	typeAuth          int32 = 3
)

const (
	// packetOverhead is everything a packet's size field counts besides the body:
	// the id and type int32s, plus two terminating NULs.
	packetOverhead = 10

	// maxPacketSize caps what one inbound packet may make us allocate. The
	// protocol's own ceiling is 4096, but the size field arrives before any of
	// the data that would corroborate it, so a confused or hostile server can
	// claim 2GB and have us reserve it. Reading generously past 4096 tolerates a
	// server that ignores the ceiling; the cap keeps that tolerance bounded.
	maxPacketSize = 64 << 10

	// maxBodyLen is the largest command body we will send, from the protocol's
	// 4096 byte packet ceiling.
	maxBodyLen = 4096 - packetOverhead
)

// ErrProtocol reports a response that is not valid Source RCON. It means the
// endpoint is not what we think it is, or the exchange has desynchronised;
// either way retrying the same way will not help.
var ErrProtocol = errors.New("rcon: malformed response")

type packet struct {
	id   int32
	typ  int32
	body string
}

// writePacket frames one packet onto w.
func writePacket(w io.Writer, typ, id int32, body string) error {
	if len(body) > maxBodyLen {
		return fmt.Errorf("rcon: body of %d bytes exceeds the %d byte limit", len(body), maxBodyLen)
	}
	// Bounded by the check above, so the conversion cannot overflow.
	size := uint32(packetOverhead + len(body)) //nolint:gosec // len(body) <= maxBodyLen

	buf := make([]byte, 0, 4+size)
	buf = binary.LittleEndian.AppendUint32(buf, size)
	// int32 to uint32 is a reinterpretation, not a range conversion: ids are
	// signed on the wire and -1 is a meaningful value.
	buf = binary.LittleEndian.AppendUint32(buf, uint32(id))  //nolint:gosec // signed id is reinterpreted, not narrowed
	buf = binary.LittleEndian.AppendUint32(buf, uint32(typ)) //nolint:gosec // signed type is reinterpreted, not narrowed
	buf = append(buf, body...)
	buf = append(buf, 0, 0)

	if _, err := w.Write(buf); err != nil {
		return fmt.Errorf("rcon: write packet: %w", err)
	}
	return nil
}

// readPacket reads one framed packet from r.
//
// Read errors are returned unwrapped so callers can tell a deadline from a
// closed connection with errors.As and errors.Is; only framing problems become
// ErrProtocol.
func readPacket(r io.Reader) (packet, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return packet{}, err
	}

	size := binary.LittleEndian.Uint32(header[:])
	switch {
	case size < packetOverhead:
		return packet{}, fmt.Errorf("%w: packet claims %d bytes, below the %d byte minimum",
			ErrProtocol, size, packetOverhead)
	case size > maxPacketSize:
		return packet{}, fmt.Errorf("%w: packet claims %d bytes, above the %d byte cap",
			ErrProtocol, size, maxPacketSize)
	}

	rest := make([]byte, size)
	if _, err := io.ReadFull(r, rest); err != nil {
		return packet{}, err
	}

	return packet{
		id:   int32(binary.LittleEndian.Uint32(rest[0:4])), //nolint:gosec // ids are signed on the wire; -1 is meaningful
		typ:  int32(binary.LittleEndian.Uint32(rest[4:8])), //nolint:gosec // types are signed on the wire
		body: string(bytes.TrimRight(rest[8:], "\x00")),
	}, nil
}
