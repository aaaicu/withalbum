package relay

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

// tcpPair returns two ends of a loopback TCP connection.
func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ch := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		ch <- c
	}()
	a, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	b := <-ch
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}

func sessionPair(t *testing.T, opener, acceptor SessionConfig) (*Session, *Session) {
	a, b := tcpPair(t)
	opener.Opener = true
	o, c := NewSession(a, nil, opener), NewSession(b, nil, acceptor)
	t.Cleanup(func() { o.Close(); c.Close() })
	return o, c
}

// TestMuxManyStreamsBothWays sends more than a window in both directions
// on many streams at once and checks every byte.
func TestMuxManyStreamsBothWays(t *testing.T) {
	o, c := sessionPair(t, SessionConfig{}, SessionConfig{})
	const streams, size = 12, 3*InitialWindow + 12345
	// Acceptor: echo each stream back, hashed.
	go func() {
		for {
			st, err := c.AcceptStream()
			if err != nil {
				return
			}
			go func() {
				io.Copy(st, st)
				st.CloseWrite()
				st.Close()
			}()
		}
	}()
	var wg sync.WaitGroup
	for i := range streams {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := o.OpenStream("192.0.2.1")
			if err != nil {
				t.Error(err)
				return
			}
			defer st.Close()
			data := make([]byte, size)
			rand.Read(data)
			go func() {
				st.Write(data)
				st.CloseWrite()
			}()
			got, err := io.ReadAll(st)
			if err != nil || sha256.Sum256(got) != sha256.Sum256(data) {
				t.Errorf("stream %d: %d bytes back, err %v", i, len(got), err)
			}
		}()
	}
	wg.Wait()
	// Streams are removed once both sides closed.
	deadline := time.Now().Add(5 * time.Second)
	for o.NumStreams()+c.NumStreams() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := o.NumStreams() + c.NumStreams(); n != 0 {
		t.Fatalf("%d streams left", n)
	}
}

// TestMuxSlowStreamDoesNotBlockOthers: a stream whose reader stalls fills
// its own window only.
func TestMuxSlowStreamDoesNotBlockOthers(t *testing.T) {
	o, c := sessionPair(t, SessionConfig{}, SessionConfig{})
	accepted := make(chan *Stream, 2)
	go func() {
		for {
			st, err := c.AcceptStream()
			if err != nil {
				return
			}
			accepted <- st
		}
	}()
	slow, _ := o.OpenStream("a")
	<-accepted // never read
	wrote := make(chan int, 1)
	go func() {
		n, _ := slow.Write(make([]byte, 2*InitialWindow))
		wrote <- n
	}()
	fast, _ := o.OpenStream("b")
	fastPeer := <-accepted
	go func() { fast.Write(bytes.Repeat([]byte("x"), 100000)); fast.CloseWrite() }()
	got, err := io.ReadAll(fastPeer)
	if err != nil || len(got) != 100000 {
		t.Fatalf("fast stream: %d %v", len(got), err)
	}
	select {
	case n := <-wrote:
		t.Fatalf("slow write finished (%d bytes) without a reader", n)
	case <-time.After(100 * time.Millisecond):
	}
	slow.SetWriteDeadline(time.Now())
	if n := <-wrote; n != InitialWindow {
		t.Fatalf("slow stream wrote %d, want exactly the window %d", n, InitialWindow)
	}
}

func TestMuxDeadlinesAndClose(t *testing.T) {
	o, c := sessionPair(t, SessionConfig{}, SessionConfig{})
	st, _ := o.OpenStream("x")
	peer, _ := c.AcceptStream()
	st.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	if _, err := st.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read deadline: %v", err)
	}
	st.SetReadDeadline(time.Time{})
	// Half close: the peer reads EOF but can still answer.
	st.Write([]byte("ping"))
	st.CloseWrite()
	if b, err := io.ReadAll(peer); string(b) != "ping" || err != nil {
		t.Fatalf("peer read %q %v", b, err)
	}
	peer.Write([]byte("pong"))
	peer.Close()
	if b, err := io.ReadAll(st); string(b) != "pong" || err != nil {
		t.Fatalf("read %q %v", b, err)
	}
	if _, err := st.Write([]byte("x")); err == nil {
		t.Fatal("write after CloseWrite")
	}
	st.Close()
	if _, err := st.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("read after close: %v", err)
	}
	// Session end resets open streams.
	st2, _ := o.OpenStream("y")
	c.AcceptStream()
	c.Close()
	if _, err := st2.Read(make([]byte, 1)); !errors.Is(err, ErrStreamReset) {
		t.Fatalf("after session end: %v", err)
	}
	<-o.Done()
}

func TestMuxMaxStreams(t *testing.T) {
	// The opener's own limit.
	o, _ := sessionPair(t, SessionConfig{MaxStreams: 2}, SessionConfig{})
	for i := range 2 {
		if _, err := o.OpenStream("x"); err != nil {
			t.Fatalf("stream %d: %v", i, err)
		}
	}
	if _, err := o.OpenStream("x"); !errors.Is(err, ErrTooManyStreams) {
		t.Fatalf("over the opener's limit: %v", err)
	}
	// The acceptor's limit: further OPENs are reset.
	o2, c2 := sessionPair(t, SessionConfig{}, SessionConfig{MaxStreams: 1})
	a, _ := o2.OpenStream("1")
	c2.AcceptStream()
	b, _ := o2.OpenStream("2")
	if _, err := b.Read(make([]byte, 1)); !errors.Is(err, ErrStreamReset) {
		t.Fatalf("over the acceptor's limit: %v", err)
	}
	a.Write([]byte("still works"))
}

// TestMuxProtocolViolations: a peer that breaks the rules loses the tunnel.
func TestMuxProtocolViolations(t *testing.T) {
	for name, frames := range map[string][][]byte{
		"data beyond the window": func() [][]byte {
			fs := [][]byte{AppendFrame(nil, FrameOpen, 1, nil)}
			for range InitialWindow/MaxDataPayload + 1 {
				fs = append(fs, AppendFrame(nil, FrameData, 1, make([]byte, MaxDataPayload)))
			}
			return fs
		}(),
		"stream never opened":   {AppendFrame(nil, FrameData, 5, []byte("x"))},
		"stream IDs going back": {AppendFrame(nil, FrameOpen, 2, nil), AppendFrame(nil, FrameOpen, 1, nil)},
		"data after fin":        {AppendFrame(nil, FrameOpen, 1, nil), AppendFrame(nil, FrameFin, 1, nil), AppendFrame(nil, FrameData, 1, []byte("x"))},
		"bad frame":             {{0xee, 0, 0, 0, 0, 0, 0, 0, 0}},
	} {
		t.Run(name, func(t *testing.T) {
			raw, b := tcpPair(t)
			c := NewSession(b, nil, SessionConfig{AcceptBacklog: 2})
			defer c.Close()
			go func() {
				for _, f := range frames {
					if _, err := raw.Write(f); err != nil {
						return
					}
				}
			}()
			select {
			case <-c.Done():
				if !errors.Is(c.Err(), ErrProtocol) {
					t.Fatalf("ended with %v", c.Err())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("violation not detected")
			}
		})
	}
}

func TestMuxPingAndIdle(t *testing.T) {
	o, c := sessionPair(t, SessionConfig{ReadTimeout: 300 * time.Millisecond}, SessionConfig{PingInterval: 50 * time.Millisecond})
	time.Sleep(600 * time.Millisecond)
	if o.Err() != nil || c.Err() != nil {
		t.Fatalf("pinged session ended: %v %v", o.Err(), c.Err())
	}
	// Without pings the reader gives up.
	o2, _ := sessionPair(t, SessionConfig{ReadTimeout: 100 * time.Millisecond}, SessionConfig{})
	select {
	case <-o2.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("idle session not closed")
	}
}
