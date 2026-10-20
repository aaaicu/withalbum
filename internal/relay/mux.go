package relay

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// Stream multiplexing over one tunnel connection.
//
// Flow control is per stream and credit based: each side may send at most
// InitialWindow bytes of DATA on a stream that the other side has not yet
// acknowledged with WINDOW frames. A receiver acknowledges bytes once the
// application has read them, in batches of at least InitialWindow/2, so it
// never buffers more than InitialWindow bytes per stream, and a slow app
// connection only slows its own stream, never the tunnel. A peer that sends
// beyond its credit breaks the protocol and the tunnel is closed.
//
// Frames are written one at a time under a mutex with a write deadline.
// The reading goroutine never blocks on a write: the control frames it
// answers with (PONG, RST) go through a small queue drained by a separate
// writer, so two peers can never deadlock by both waiting to write.

// InitialWindow is the per-stream credit in each direction. It is fixed by
// the protocol (no negotiation).
const InitialWindow = 256 << 10

// maxSendWindow bounds the credit a peer can grant (protocol error above).
const maxSendWindow = 1 << 30

// maxControlQueue bounds queued control frames; a peer that makes more
// pending answers than this is not reading and the tunnel is closed.
const maxControlQueue = 1024

var (
	// ErrSessionClosed: the tunnel is gone.
	ErrSessionClosed = errors.New("relay: tunnel closed")
	// ErrStreamReset: the peer aborted the stream.
	ErrStreamReset = errors.New("relay: stream reset by peer")
	// ErrTooManyStreams: the tunnel has its maximum number of open streams.
	ErrTooManyStreams = errors.New("relay: too many streams")
)

// SessionConfig configures a Session. Zero values take the defaults.
type SessionConfig struct {
	// Opener: this side opens streams (the relay); the other accepts them
	// (the home server).
	Opener bool
	// MaxStreams open at once (default 64). The accepting side refuses
	// further OPENs with RST; the opening side returns ErrTooManyStreams.
	MaxStreams int
	// ReadTimeout: no frame at all for this long closes the session
	// (default 75 s). Keep it above the peer's ping interval.
	ReadTimeout time.Duration
	// WriteTimeout bounds one frame write (default 30 s).
	WriteTimeout time.Duration
	// PingInterval: send PING this often (0: never).
	PingInterval time.Duration
	// AcceptBacklog: opened streams waiting for AcceptStream (default 16);
	// beyond it new streams are refused with RST.
	AcceptBacklog int
	// Linger: after Close, how long to wait for the peer's FIN before the
	// stream is aborted with RST (default 30 s).
	Linger time.Duration
}

func (c SessionConfig) withDefaults() SessionConfig {
	if c.MaxStreams <= 0 {
		c.MaxStreams = 64
	}
	if c.ReadTimeout <= 0 {
		c.ReadTimeout = 75 * time.Second
	}
	if c.WriteTimeout <= 0 {
		c.WriteTimeout = 30 * time.Second
	}
	if c.AcceptBacklog <= 0 {
		c.AcceptBacklog = 16
	}
	if c.Linger <= 0 {
		c.Linger = 30 * time.Second
	}
	return c
}

// Session is one end of a tunnel.
type Session struct {
	cfg  SessionConfig
	conn net.Conn
	fr   *FrameReader

	wmu  sync.Mutex // serializes frame writes
	wbuf []byte

	ctrlMu  sync.Mutex
	ctrl    [][]byte
	ctrlSig chan struct{}

	mu       sync.Mutex
	streams  map[uint32]*Stream
	nextID   uint32 // opener: next stream ID
	lastPeer uint32 // acceptor: highest stream ID opened by the peer
	accept   chan *Stream

	done chan struct{}
	once sync.Once
	err  error
}

// NewSession starts a session on conn after the handshake. r reads the
// rest of the connection (a bufio.Reader that may hold buffered bytes); nil
// reads conn directly.
func NewSession(conn net.Conn, r io.Reader, cfg SessionConfig) *Session {
	if r == nil {
		r = conn
	}
	s := &Session{cfg: cfg.withDefaults(), conn: conn, fr: NewFrameReader(r), ctrlSig: make(chan struct{}, 1),
		streams: map[uint32]*Stream{}, nextID: 1, done: make(chan struct{})}
	s.accept = make(chan *Stream, s.cfg.AcceptBacklog)
	go s.readLoop()
	go s.controlLoop()
	if s.cfg.PingInterval > 0 {
		go s.pingLoop()
	}
	return s
}

// Done is closed when the session ends.
func (s *Session) Done() <-chan struct{} { return s.done }

// Err is why the session ended (nil while it runs).
func (s *Session) Err() error {
	select {
	case <-s.done:
		return s.err
	default:
		return nil
	}
}

// Close ends the session and every stream.
func (s *Session) Close() error {
	s.fail(ErrSessionClosed)
	return nil
}

// NumStreams is the number of streams not yet removed.
func (s *Session) NumStreams() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.streams)
}

func (s *Session) fail(err error) {
	s.once.Do(func() {
		s.err = err
		close(s.done)
		s.conn.Close()
		s.mu.Lock()
		streams := s.streams
		s.streams = map[uint32]*Stream{}
		s.mu.Unlock()
		for _, st := range streams {
			st.kill()
		}
	})
}

// writeFrame writes one frame, blocking until it is on the connection. A
// write error ends the session.
func (s *Session) writeFrame(typ byte, id uint32, payload []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	select {
	case <-s.done:
		return ErrSessionClosed
	default:
	}
	s.wbuf = AppendFrame(s.wbuf[:0], typ, id, payload)
	return s.writeRaw(s.wbuf)
}

// writeRaw writes encoded frames; wmu must be held.
func (s *Session) writeRaw(b []byte) error {
	s.conn.SetWriteDeadline(time.Now().Add(s.cfg.WriteTimeout))
	if _, err := s.conn.Write(b); err != nil {
		s.fail(fmt.Errorf("relay: tunnel write: %w", err))
		return ErrSessionClosed
	}
	return nil
}

// queueControl queues a control frame without blocking (read loop only).
func (s *Session) queueControl(typ byte, id uint32, payload []byte) error {
	s.ctrlMu.Lock()
	if len(s.ctrl) >= maxControlQueue {
		s.ctrlMu.Unlock()
		return fmt.Errorf("%w: peer does not read control frames", ErrProtocol)
	}
	s.ctrl = append(s.ctrl, AppendFrame(nil, typ, id, payload))
	s.ctrlMu.Unlock()
	select {
	case s.ctrlSig <- struct{}{}:
	default:
	}
	return nil
}

func (s *Session) controlLoop() {
	for {
		select {
		case <-s.done:
			return
		case <-s.ctrlSig:
		}
		s.ctrlMu.Lock()
		batch := s.ctrl
		s.ctrl = nil
		s.ctrlMu.Unlock()
		if len(batch) == 0 {
			continue
		}
		var b []byte
		for _, f := range batch {
			b = append(b, f...)
		}
		s.wmu.Lock()
		err := s.writeRaw(b)
		s.wmu.Unlock()
		if err != nil {
			return
		}
	}
}

func (s *Session) pingLoop() {
	t := time.NewTicker(s.cfg.PingInterval)
	defer t.Stop()
	var payload [8]byte
	for n := uint64(1); ; n++ {
		select {
		case <-s.done:
			return
		case <-t.C:
		}
		binary.BigEndian.PutUint64(payload[:], n)
		if s.writeFrame(FramePing, 0, payload[:]) != nil {
			return
		}
	}
}

func (s *Session) readLoop() {
	for {
		s.conn.SetReadDeadline(time.Now().Add(s.cfg.ReadTimeout))
		f, err := s.fr.ReadFrame()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				err = fmt.Errorf("relay: tunnel idle for %s", s.cfg.ReadTimeout)
			}
			s.fail(err)
			return
		}
		if err := s.handle(f); err != nil {
			s.fail(err)
			return
		}
	}
}

// known reports whether id was ever opened on this session.
func (s *Session) known(id uint32) bool {
	if s.cfg.Opener {
		return id >= 1 && id < s.nextID
	}
	return id >= 1 && id <= s.lastPeer
}

func (s *Session) handle(f Frame) error {
	switch f.Type {
	case FramePing:
		return s.queueControl(FramePong, 0, f.Payload)
	case FramePong:
		return nil
	case FrameOpen:
		return s.handleOpen(f)
	}
	s.mu.Lock()
	st := s.streams[f.Stream]
	known := s.known(f.Stream)
	s.mu.Unlock()
	if st == nil {
		if !known {
			return fmt.Errorf("%w: frame for stream %d that was never opened", ErrProtocol, f.Stream)
		}
		// A stream this side already removed: tell the peer to stop.
		if f.Type == FrameData {
			return s.queueControl(FrameRst, f.Stream, nil)
		}
		return nil
	}
	switch f.Type {
	case FrameData:
		return st.recvData(f.Payload)
	case FrameWindow:
		return st.recvWindow(binary.BigEndian.Uint32(f.Payload))
	case FrameFin:
		st.recvFin()
	case FrameRst:
		st.recvRst()
	}
	return nil
}

func (s *Session) handleOpen(f Frame) error {
	if s.cfg.Opener {
		return fmt.Errorf("%w: OPEN sent to the opening side", ErrProtocol)
	}
	s.mu.Lock()
	if f.Stream <= s.lastPeer {
		s.mu.Unlock()
		return fmt.Errorf("%w: OPEN of stream %d after %d", ErrProtocol, f.Stream, s.lastPeer)
	}
	s.lastPeer = f.Stream
	if len(s.streams) >= s.cfg.MaxStreams {
		s.mu.Unlock()
		return s.queueControl(FrameRst, f.Stream, nil)
	}
	st := newStream(s, f.Stream, string(f.Payload))
	s.streams[f.Stream] = st
	s.mu.Unlock()
	select {
	case s.accept <- st:
		return nil
	default:
		s.remove(f.Stream)
		return s.queueControl(FrameRst, f.Stream, nil)
	}
}

// OpenStream opens a stream (opening side only). remote is passed to the
// peer in the OPEN frame (the app's address).
func (s *Session) OpenStream(remote string) (*Stream, error) {
	if !s.cfg.Opener {
		return nil, errors.New("relay: only the opening side opens streams")
	}
	if len(remote) > MaxOpenPayload {
		remote = remote[:MaxOpenPayload]
	}
	// IDs must reach the peer in increasing order: assign the ID and write
	// the OPEN frame under the write lock (lock order: wmu, then mu).
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.mu.Lock()
	select {
	case <-s.done:
		s.mu.Unlock()
		return nil, ErrSessionClosed
	default:
	}
	if len(s.streams) >= s.cfg.MaxStreams {
		s.mu.Unlock()
		return nil, ErrTooManyStreams
	}
	id := s.nextID
	s.nextID++
	st := newStream(s, id, remote)
	s.streams[id] = st
	s.mu.Unlock()
	s.wbuf = AppendFrame(s.wbuf[:0], FrameOpen, id, []byte(remote))
	if err := s.writeRaw(s.wbuf); err != nil {
		return nil, err
	}
	return st, nil
}

// AcceptStream waits for the next stream the peer opens (accepting side).
func (s *Session) AcceptStream() (*Stream, error) {
	select {
	case st := <-s.accept:
		return st, nil
	case <-s.done:
		return nil, s.err
	}
}

func (s *Session) remove(id uint32) {
	s.mu.Lock()
	delete(s.streams, id)
	s.mu.Unlock()
}

func (s *Session) has(st *Stream) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streams[st.id] == st
}

// Addr is the address of a stream end: network "relay".
type Addr struct{ S string }

func (Addr) Network() string  { return "relay" }
func (a Addr) String() string { return a.S }

// Stream is one multiplexed connection. It implements net.Conn, including
// deadlines and CloseWrite (half close).
type Stream struct {
	s      *Session
	id     uint32
	remote string
	raddr  net.Addr

	mu          sync.Mutex
	rbuf        []byte // received, not yet read
	recvCredit  int    // bytes the peer may still send
	unacked     int    // read by the application, not yet acknowledged
	sendCredit  int64  // bytes this side may still send
	remoteFin   bool
	localFin    bool
	reset       bool // aborted (by RST or session end)
	closedLocal bool
	rdl, wdl    time.Time
	rsig, wsig  chan struct{}
	term        chan struct{} // closed on reset or local close
	termOnce    sync.Once
}

func newStream(s *Session, id uint32, remote string) *Stream {
	return &Stream{s: s, id: id, remote: remote, raddr: Addr{S: remote}, recvCredit: InitialWindow, sendCredit: InitialWindow,
		rsig: make(chan struct{}, 1), wsig: make(chan struct{}, 1), term: make(chan struct{})}
}

// ID is the stream ID within its tunnel.
func (st *Stream) ID() uint32 { return st.id }

// Remote is the text of the peer's OPEN frame (the client address).
func (st *Stream) Remote() string { return st.remote }

// SetRemoteAddr replaces what RemoteAddr returns (before use).
func (st *Stream) SetRemoteAddr(a net.Addr) { st.raddr = a }

func (st *Stream) LocalAddr() net.Addr  { return Addr{S: "tunnel"} }
func (st *Stream) RemoteAddr() net.Addr { return st.raddr }

func signal(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

func (st *Stream) closeTerm() { st.termOnce.Do(func() { close(st.term) }) }

// kill aborts the stream because the session ended.
func (st *Stream) kill() {
	st.mu.Lock()
	st.reset = true
	st.mu.Unlock()
	st.closeTerm()
}

func (st *Stream) recvData(p []byte) error {
	st.mu.Lock()
	if len(p) > st.recvCredit {
		st.mu.Unlock()
		return fmt.Errorf("%w: stream %d: %d bytes beyond the window", ErrProtocol, st.id, len(p))
	}
	if st.remoteFin {
		st.mu.Unlock()
		return fmt.Errorf("%w: stream %d: DATA after FIN", ErrProtocol, st.id)
	}
	if st.closedLocal || st.reset {
		// Nobody will read it: abort the stream so the peer stops.
		st.reset = true
		st.mu.Unlock()
		st.closeTerm()
		st.s.remove(st.id)
		return st.s.queueControl(FrameRst, st.id, nil)
	}
	st.recvCredit -= len(p)
	st.rbuf = append(st.rbuf, p...)
	st.mu.Unlock()
	signal(st.rsig)
	return nil
}

func (st *Stream) recvWindow(inc uint32) error {
	st.mu.Lock()
	st.sendCredit += int64(inc)
	over := st.sendCredit > maxSendWindow
	st.mu.Unlock()
	if over {
		return fmt.Errorf("%w: stream %d: window above %d", ErrProtocol, st.id, maxSendWindow)
	}
	signal(st.wsig)
	return nil
}

func (st *Stream) recvFin() {
	st.mu.Lock()
	st.remoteFin = true
	done := st.closedLocal
	st.mu.Unlock()
	signal(st.rsig)
	if done {
		st.s.remove(st.id)
	}
}

func (st *Stream) recvRst() {
	st.mu.Lock()
	st.reset = true
	st.mu.Unlock()
	st.closeTerm()
	st.s.remove(st.id)
}

// wait blocks until c is signalled, the stream terminates, the session
// ends or the deadline dl passes (false).
func (st *Stream) wait(c chan struct{}, dl time.Time) bool {
	var timeout <-chan time.Time
	if !dl.IsZero() {
		d := time.Until(dl)
		if d <= 0 {
			return false
		}
		t := time.NewTimer(d)
		defer t.Stop()
		timeout = t.C
	}
	select {
	case <-c:
	case <-st.term:
	case <-st.s.done:
	case <-timeout:
		return false
	}
	return true
}

func (st *Stream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		st.mu.Lock()
		switch {
		case st.closedLocal:
			st.mu.Unlock()
			return 0, net.ErrClosed
		case len(st.rbuf) > 0:
			n := copy(p, st.rbuf)
			st.rbuf = st.rbuf[n:]
			if len(st.rbuf) == 0 {
				st.rbuf = nil
			}
			st.unacked += n
			var ack int
			if st.unacked >= InitialWindow/2 && !st.remoteFin {
				ack, st.unacked = st.unacked, 0
				st.recvCredit += ack
			}
			st.mu.Unlock()
			if ack > 0 {
				var b [4]byte
				binary.BigEndian.PutUint32(b[:], uint32(ack))
				st.s.writeFrame(FrameWindow, st.id, b[:])
			}
			return n, nil
		case st.reset:
			st.mu.Unlock()
			return 0, ErrStreamReset
		case st.remoteFin:
			st.mu.Unlock()
			return 0, io.EOF
		}
		dl := st.rdl
		st.mu.Unlock()
		if !st.wait(st.rsig, dl) {
			return 0, os.ErrDeadlineExceeded
		}
		select {
		case <-st.s.done:
			st.kill()
		default:
		}
	}
}

func (st *Stream) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		st.mu.Lock()
		switch {
		case st.reset:
			st.mu.Unlock()
			return total, ErrStreamReset
		case st.closedLocal || st.localFin:
			st.mu.Unlock()
			return total, net.ErrClosed
		case st.sendCredit > 0:
			n := int(min(int64(len(p)), st.sendCredit, MaxDataPayload))
			st.sendCredit -= int64(n)
			st.mu.Unlock()
			if err := st.s.writeFrame(FrameData, st.id, p[:n]); err != nil {
				return total, err
			}
			total += n
			p = p[n:]
			continue
		}
		dl := st.wdl
		st.mu.Unlock()
		if !st.wait(st.wsig, dl) {
			return total, os.ErrDeadlineExceeded
		}
		select {
		case <-st.s.done:
			st.kill()
		default:
		}
	}
	return total, nil
}

// CloseWrite sends FIN: the peer reads EOF after the data sent so far.
// Reading continues.
func (st *Stream) CloseWrite() error {
	st.mu.Lock()
	send := !st.localFin && !st.reset && !st.closedLocal
	st.localFin = true
	st.mu.Unlock()
	if send {
		return st.s.writeFrame(FrameFin, st.id, nil)
	}
	return nil
}

// Close ends the stream: FIN if not sent yet, unread data is dropped. It
// is removed from the tunnel when the peer's FIN arrives, or aborted with
// RST after the linger time.
func (st *Stream) Close() error {
	st.mu.Lock()
	if st.closedLocal {
		st.mu.Unlock()
		return nil
	}
	st.closedLocal = true
	st.rbuf = nil
	sendFin := !st.localFin && !st.reset
	st.localFin = true
	done := st.remoteFin || st.reset
	st.mu.Unlock()
	st.closeTerm()
	if sendFin {
		st.s.writeFrame(FrameFin, st.id, nil)
	}
	if done {
		st.s.remove(st.id)
		return nil
	}
	time.AfterFunc(st.s.cfg.Linger, func() {
		if st.s.has(st) {
			st.s.remove(st.id)
			st.s.writeFrame(FrameRst, st.id, nil)
		}
	})
	return nil
}

func (st *Stream) SetDeadline(t time.Time) error {
	st.SetReadDeadline(t)
	return st.SetWriteDeadline(t)
}

func (st *Stream) SetReadDeadline(t time.Time) error {
	st.mu.Lock()
	st.rdl = t
	st.mu.Unlock()
	signal(st.rsig)
	return nil
}

func (st *Stream) SetWriteDeadline(t time.Time) error {
	st.mu.Lock()
	st.wdl = t
	st.mu.Unlock()
	signal(st.wsig)
	return nil
}
