package network

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

// Server represents a server which can serve requests.
type Server interface {
	Serve(listener net.Listener)
}

// ErrorHandler handles an error and notifies the listener on whether
// it should continue serving.
type ErrorHandler func(error) bool

type errListenerClosed string

func (e errListenerClosed) Error() string   { return string(e) }
func (e errListenerClosed) Temporary() bool { return false }
func (e errListenerClosed) Timeout() bool   { return false }

// ErrListenerClosed is returned from muxListener.Accept when the underlying
// listener is closed.
var ErrListenerClosed = errListenerClosed("mux: listener closed")

// for readability of readTimeout
var noTimeout time.Duration

// New announces on the local network address laddr. The syntax of laddr is
// "host:port", like "127.0.0.1:8080". If host is omitted, as in ":8080",
// New listens on all available interfaces instead of just the interface
// with the given host address. Listening on a hostname is not recommended
// because this creates a socket for at most one of its IP addresses.
func New(address string) (*Listener, error) {
	l, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}

	return &Listener{
		root:         l,
		bufferSize:   1024,
		errorHandler: func(_ error) bool { return true },
		closing:      make(chan struct{}),
		readTimeout:  noTimeout,
	}, nil
}

// Listener represents a listener used for multiplexing protocols.
type Listener struct {
	root         net.Listener
	bufferSize   int
	errorHandler ErrorHandler
	closing      chan struct{}
	readTimeout  time.Duration
}

// Accept waits for and returns the next connection to the listener.
func (m *Listener) Accept() (net.Conn, error) {
	return m.root.Accept()
}

// ServeAsync adds a protocol based on the matcher and serves it.
func (m *Listener) ServeAsync(serve func(l net.Listener) error) {
	ml := muxListener{
		Listener:    m.root,
		connections: make(chan net.Conn, m.bufferSize),
	}
	go serve(ml)
}

// SetReadTimeout sets a timeout for the read of matchers.
func (m *Listener) SetReadTimeout(t time.Duration) {
	m.readTimeout = t
}

// Serve starts multiplexing the listener.
func (m *Listener) Serve() error {
	var wg sync.WaitGroup

	defer func() {
		close(m.closing)
		wg.Wait()

		// TODO: drain the connections aequneued for the listener.
	}()

	for {
		c, err := m.root.Accept()
		if err != nil {
			if !m.handleErr(err) {
				return err
			}
			continue
		}

		wg.Add(1)
		go m.serve(c, m.closing, &wg)
	}
}

func (m *Listener) serve(c net.Conn, donec <-chan struct{}, wg *sync.WaitGroup) {
	defer wg.Done()

	_ = c.Close()
	err := ErrNotMatched{c: c}
	if !m.handleErr(err) {
		_ = m.root.Close()
	}
}

// HandleError registers an error handler that handles listener errors.
func (m *Listener) HandleError(h ErrorHandler) {
	m.errorHandler = h
}

func (m *Listener) handleErr(err error) bool {
	if !m.errorHandler(err) {
		return false
	}

	if ne, ok := err.(net.Error); ok {
		return ne.Temporary()
	}

	return false
}

// Close closes the listener
func (m *Listener) Close() error {
	return m.root.Close()
}

// ------------------------------------------------------------------------------------

type muxListener struct {
	net.Listener
	connections chan net.Conn
}

func (l muxListener) Accept() (net.Conn, error) {
	c, ok := <-l.connections
	if !ok {
		return nil, ErrListenerClosed
	}
	return c, nil
}

// ------------------------------------------------------------------------------------

// Conn wraps a net.Conn and provides transparent sniffing of connection data.
type Conn struct {
	net.Conn
	buffer sniffer
}

// NewConn creates a new sniffed connection.
func newConn(c net.Conn) *Conn {
	return &Conn{
		Conn:   c,
		buffer: sniffer{source: c},
	}
}

// Read reads the block of data from the underlying buffer.
func (m *Conn) Read(p []byte) (int, error) {
	return m.buffer.Read(p)
}

func (m *Conn) startSniffing() io.Reader {
	m.buffer.reset(true)
	return &m.buffer
}

func (m *Conn) doneSniffing() {
	m.buffer.reset(false)
}

// ------------------------------------------------------------------------------------

// Sniffer represents a io.Reader which can peek incoming bytes and reset back to normal.
type sniffer struct {
	source     io.Reader
	buffer     bytes.Buffer
	bufferRead int
	bufferSize int
	sniffing   bool
	lastErr    error
}

// Read reads data from the buffer.
func (s *sniffer) Read(p []byte) (int, error) {
	if s.bufferSize > s.bufferRead {
		bn := copy(p, s.buffer.Bytes()[s.bufferRead:s.bufferSize])
		s.bufferRead += bn
		return bn, s.lastErr
	} else if !s.sniffing && s.buffer.Cap() != 0 {
		s.buffer = bytes.Buffer{}
	}

	sn, sErr := s.source.Read(p)
	if sn > 0 && s.sniffing {
		s.lastErr = sErr
		if wn, wErr := s.buffer.Write(p[:sn]); wErr != nil {
			return wn, wErr
		}
	}
	return sn, sErr
}

// Reset resets the buffer.
func (s *sniffer) reset(snif bool) {
	s.sniffing = snif
	s.bufferRead = 0
	s.bufferSize = s.buffer.Len()
}