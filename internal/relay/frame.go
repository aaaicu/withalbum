package relay

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Tunnel frames. After the SERVE handshake both ends exchange frames:
//
//	+------+-----------+-----------+---------------+
//	| type | stream ID | length    | payload       |
//	| 1 B  | 4 B (BE)  | 4 B (BE)  | length bytes  |
//	+------+-----------+-----------+---------------+
//
// Stream 0 is the connection itself (PING/PONG only); the relay opens
// streams with IDs 1, 2, 3, ... and never reuses one within a tunnel.
// Each type has a fixed or bounded payload size, checked before the
// payload is read, so a peer can never make the reader allocate or wait
// for more than MaxDataPayload bytes.
const (
	FrameData   byte = 0 // stream bytes, 1..MaxDataPayload
	FrameWindow byte = 1 // receive credit: uint32 increment (> 0)
	FrameOpen   byte = 2 // relay -> server: new stream; payload: client address text (0..MaxOpenPayload)
	FrameFin    byte = 3 // the sender will send no more data on the stream (half close)
	FrameRst    byte = 4 // the stream is aborted in both directions
	FramePing   byte = 5 // stream 0, 8 opaque bytes; answered by PONG with the same bytes
	FramePong   byte = 6
)

const (
	// HeaderLen is the size of a frame header.
	HeaderLen = 9
	// MaxDataPayload is the largest DATA payload.
	MaxDataPayload = 16 << 10
	// MaxOpenPayload bounds the client address carried by OPEN.
	MaxOpenPayload = 64
)

// ErrProtocol: the peer sent a frame that breaks the rules; the tunnel is
// closed.
var ErrProtocol = errors.New("relay: protocol violation")

// Frame is a decoded frame. Payload aliases the reader's buffer.
type Frame struct {
	Type    byte
	Stream  uint32
	Payload []byte
}

// checkHeader validates a header before its payload is read.
func checkHeader(typ byte, stream uint32, n uint32) error {
	bad := func(why string) error {
		return fmt.Errorf("%w: frame type %d stream %d length %d: %s", ErrProtocol, typ, stream, n, why)
	}
	switch typ {
	case FrameData:
		if stream == 0 || n == 0 || n > MaxDataPayload {
			return bad("bad DATA")
		}
	case FrameWindow:
		if stream == 0 || n != 4 {
			return bad("bad WINDOW")
		}
	case FrameOpen:
		if stream == 0 || n > MaxOpenPayload {
			return bad("bad OPEN")
		}
	case FrameFin, FrameRst:
		if stream == 0 || n != 0 {
			return bad("bad FIN/RST")
		}
	case FramePing, FramePong:
		if stream != 0 || n != 8 {
			return bad("bad PING/PONG")
		}
	default:
		return bad("unknown type")
	}
	return nil
}

// FrameReader reads frames from r into a reused buffer.
type FrameReader struct {
	r   io.Reader
	hdr [HeaderLen]byte
	buf [MaxDataPayload]byte
}

// NewFrameReader returns a reader of the frames of r.
func NewFrameReader(r io.Reader) *FrameReader { return &FrameReader{r: r} }

// ReadFrame reads the next frame. Its payload is valid until the next call.
// A WINDOW frame with a zero increment is a protocol error.
func (fr *FrameReader) ReadFrame() (Frame, error) {
	if _, err := io.ReadFull(fr.r, fr.hdr[:]); err != nil {
		return Frame{}, err
	}
	f := Frame{Type: fr.hdr[0], Stream: binary.BigEndian.Uint32(fr.hdr[1:5])}
	n := binary.BigEndian.Uint32(fr.hdr[5:9])
	if err := checkHeader(f.Type, f.Stream, n); err != nil {
		return Frame{}, err
	}
	f.Payload = fr.buf[:n]
	if _, err := io.ReadFull(fr.r, f.Payload); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return Frame{}, err
	}
	if f.Type == FrameWindow && binary.BigEndian.Uint32(f.Payload) == 0 {
		return Frame{}, fmt.Errorf("%w: zero WINDOW increment", ErrProtocol)
	}
	return f, nil
}

// AppendFrame appends the encoding of a frame to b. It does not validate.
func AppendFrame(b []byte, typ byte, stream uint32, payload []byte) []byte {
	b = append(b, typ)
	b = binary.BigEndian.AppendUint32(b, stream)
	b = binary.BigEndian.AppendUint32(b, uint32(len(payload)))
	return append(b, payload...)
}
