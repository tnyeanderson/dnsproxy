package upstream

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/url"
	"runtime"
	"sync"
	"time"

	"github.com/AdguardTeam/dnsproxy/proxyutil"
	"github.com/AdguardTeam/golibs/errors"
	"github.com/AdguardTeam/golibs/log"
	"github.com/miekg/dns"
	"github.com/quic-go/quic-go"
)

const (
	// QUICCodeNoError is used when the connection or stream needs to be closed,
	// but there is no error to signal.
	QUICCodeNoError = quic.ApplicationErrorCode(0)

	// QUICCodeInternalError signals that the DoQ implementation encountered
	// an internal error and is incapable of pursuing the transaction or the
	// connection.
	QUICCodeInternalError = quic.ApplicationErrorCode(1)

	// QUICKeepAlivePeriod is the value that we pass to *quic.Config and that
	// controls the period with with keep-alive frames are being sent to the
	// connection. We set it to 20s as it would be in the quic-go@v0.27.1 with
	// KeepAlive field set to true This value is specified in
	// https://pkg.go.dev/github.com/quic-go/quic-go/internal/protocol#MaxKeepAliveInterval.
	//
	// TODO(ameshkov):  Consider making it configurable.
	QUICKeepAlivePeriod = time.Second * 20

	// NextProtoDQ is the ALPN token for DoQ. During the connection establishment,
	// DNS/QUIC support is indicated by selecting the ALPN token "doq" in the
	// crypto handshake.
	//
	// See https://datatracker.ietf.org/doc/rfc9250.
	NextProtoDQ = "doq"
)

// compatProtoDQ is a list of ALPN tokens used by a QUIC connection.
// NextProtoDQ is the latest draft version supported by dnsproxy, but it also
// includes previous drafts.
var compatProtoDQ = []string{NextProtoDQ, "doq-i00", "dq", "doq-i02"}

// dnsOverQUIC implements the [Upstream] interface for the DNS-over-QUIC
// protocol (spec: https://www.rfc-editor.org/rfc/rfc9250.html).
type dnsOverQUIC struct {
	// getDialer either returns an initialized dial handler or creates a new
	// one.
	getDialer DialerInitializer

	// addr is the DNS-over-QUIC server URL.
	addr *url.URL

	// tlsConf is the configuration of TLS.
	tlsConf *tls.Config

	// quicConfig is the QUIC configuration that is used for establishing
	// connections to the upstream.  This configuration includes the TokenStore
	// that needs to be stored for the lifetime of dnsOverQUIC since we can
	// re-create the connection.
	quicConfig *quic.Config

	// conn is the current active QUIC connection.  It can be closed and
	// re-opened when needed.
	conn quic.Connection

	// bytesPool is a *sync.Pool we use to store byte buffers in.  These byte
	// buffers are used to read responses from the upstream.
	bytesPool *sync.Pool

	// quicConfigMu protects quicConfig.
	quicConfigMu sync.Mutex

	// connMu protects conn.
	connMu sync.RWMutex

	// bytesPoolGuard protects bytesPool.
	bytesPoolMu sync.Mutex

	// timeout is the timeout for the upstream connection.
	timeout time.Duration
}

// type check
var _ Upstream = (*dnsOverQUIC)(nil)

// newDoQ returns the DNS-over-QUIC Upstream.
func newDoQ(addr *url.URL, opts *Options) (u Upstream, err error) {
	addPort(addr, defaultPortDoQ)

	getDialer, err := newDialerInitializer(addr, opts)
	if err != nil {
		return nil, err
	}

	u = &dnsOverQUIC{
		getDialer: getDialer,
		addr:      addr,
		quicConfig: &quic.Config{
			KeepAlivePeriod: QUICKeepAlivePeriod,
			TokenStore:      newQUICTokenStore(),
			Tracer:          opts.QUICTracer,
		},
		tlsConf: &tls.Config{
			ServerName:   addr.Hostname(),
			RootCAs:      RootCAs,
			CipherSuites: CipherSuites,
			// Use the default capacity for the LRU cache.  It may be useful to
			// store several caches since the user may be routed to different
			// servers in case there's load balancing on the server-side.
			ClientSessionCache:    tls.NewLRUClientSessionCache(0),
			MinVersion:            tls.VersionTLS12,
			InsecureSkipVerify:    opts.InsecureSkipVerify,
			VerifyPeerCertificate: opts.VerifyServerCertificate,
			VerifyConnection:      opts.VerifyConnection,
			NextProtos:            compatProtoDQ,
		},
		timeout: opts.Timeout,
	}

	runtime.SetFinalizer(u, (*dnsOverQUIC).Close)

	return u, nil
}

// Address implements the [Upstream] interface for *dnsOverQUIC.
func (p *dnsOverQUIC) Address() string { return p.addr.String() }

// Exchange implements the [Upstream] interface for *dnsOverQUIC.
func (p *dnsOverQUIC) Exchange(m *dns.Msg) (resp *dns.Msg, err error) {
	// When sending queries over a QUIC connection, the DNS Message ID MUST be
	// set to zero.
	id := m.Id
	m.Id = 0
	defer func() {
		// Restore the original ID to not break compatibility with proxies.
		m.Id = id
		if resp != nil {
			resp.Id = id
		}
	}()

	// Check if there was already an active conn before sending the request.
	// We'll only attempt to re-connect if there was one.
	hasConnection := p.hasConnection()

	// Make the first attempt to send the DNS query.
	resp, err = p.exchangeQUIC(m)

	// Make up to 2 attempts to re-open the QUIC connection and send the request
	// again.  There are several cases where this workaround is necessary to
	// make DoQ usable.  We need to make 2 attempts in the case when the
	// connection was closed (due to inactivity for example) AND the server
	// refuses to open a 0-RTT connection.
	for i := 0; hasConnection && p.shouldRetry(err) && i < 2; i++ {
		log.Debug("re-creating the QUIC connection and retrying due to %v", err)

		// Close the active connection to make sure we'll try to re-connect.
		p.closeConnWithError(err)

		// Retry sending the request.
		resp, err = p.exchangeQUIC(m)
	}

	if err != nil {
		// If we're unable to exchange messages, make sure the connection is
		// closed and signal about an internal error.
		p.closeConnWithError(err)
	}

	return resp, err
}

// Close implements the [Upstream] interface for *dnsOverQUIC.
func (p *dnsOverQUIC) Close() (err error) {
	p.connMu.Lock()
	defer p.connMu.Unlock()

	runtime.SetFinalizer(p, nil)

	if p.conn != nil {
		err = p.conn.CloseWithError(QUICCodeNoError, "")
	}

	return err
}

// exchangeQUIC attempts to open a QUIC connection, send the DNS message
// through it and return the response it got from the server.
func (p *dnsOverQUIC) exchangeQUIC(m *dns.Msg) (resp *dns.Msg, err error) {
	var conn quic.Connection
	conn, err = p.getConnection(true)
	if err != nil {
		return nil, err
	}

	var buf []byte
	buf, err = m.Pack()
	if err != nil {
		return nil, fmt.Errorf("failed to pack DNS message for DoQ: %w", err)
	}

	var stream quic.Stream
	stream, err = p.openStream(conn)
	if err != nil {
		return nil, err
	}

	_, err = stream.Write(proxyutil.AddPrefix(buf))
	if err != nil {
		return nil, fmt.Errorf("failed to write to a QUIC stream: %w", err)
	}

	// The client MUST send the DNS query over the selected stream, and MUST
	// indicate through the STREAM FIN mechanism that no further data will
	// be sent on that stream. Note, that stream.Close() closes the
	// write-direction of the stream, but does not prevent reading from it.
	_ = stream.Close()

	return p.readMsg(stream)
}

// shouldRetry checks what error we received and decides whether it is required
// to re-open the connection and retry sending the request.
func (p *dnsOverQUIC) shouldRetry(err error) (ok bool) {
	return isQUICRetryError(err)
}

// getBytesPool returns (creates if needed) a pool we store byte buffers in.
func (p *dnsOverQUIC) getBytesPool() (pool *sync.Pool) {
	p.bytesPoolMu.Lock()
	defer p.bytesPoolMu.Unlock()

	if p.bytesPool == nil {
		p.bytesPool = &sync.Pool{
			New: func() interface{} {
				b := make([]byte, dns.MaxMsgSize)

				return &b
			},
		}
	}

	return p.bytesPool
}

// getConnection opens or returns an existing quic.Connection. useCached
// argument controls whether we should try to use the existing cached
// connection.  If it is false, we will forcibly create a new connection and
// close the existing one if needed.
func (p *dnsOverQUIC) getConnection(useCached bool) (quic.Connection, error) {
	var conn quic.Connection
	p.connMu.RLock()
	conn = p.conn
	if conn != nil && useCached {
		p.connMu.RUnlock()

		return conn, nil
	}
	if conn != nil {
		// we're recreating the connection, let's create a new one.
		_ = conn.CloseWithError(QUICCodeNoError, "")
	}
	p.connMu.RUnlock()

	p.connMu.Lock()
	defer p.connMu.Unlock()

	var err error
	conn, err = p.openConnection()
	if err != nil {
		return nil, err
	}
	p.conn = conn

	return conn, nil
}

// hasConnection returns true if there's an active QUIC connection.
func (p *dnsOverQUIC) hasConnection() (ok bool) {
	p.connMu.Lock()
	defer p.connMu.Unlock()

	return p.conn != nil
}

// getQUICConfig returns the QUIC config in a thread-safe manner.  Note, that
// this method returns a pointer, it is forbidden to change its properties.
func (p *dnsOverQUIC) getQUICConfig() (c *quic.Config) {
	p.quicConfigMu.Lock()
	defer p.quicConfigMu.Unlock()

	return p.quicConfig
}

// resetQUICConfig re-creates the tokens store as we may need to use a new one
// if we failed to connect.
func (p *dnsOverQUIC) resetQUICConfig() {
	p.quicConfigMu.Lock()
	defer p.quicConfigMu.Unlock()

	p.quicConfig = p.quicConfig.Clone()
	p.quicConfig.TokenStore = newQUICTokenStore()
}

// openStream opens a new QUIC stream for the specified connection.
func (p *dnsOverQUIC) openStream(conn quic.Connection) (quic.Stream, error) {
	ctx, cancel := p.withDeadline(context.Background())
	defer cancel()

	stream, err := conn.OpenStreamSync(ctx)
	if err == nil {
		return stream, nil
	}

	// We can get here if the old QUIC connection is not valid anymore.  We
	// should try to re-create the connection again in this case.
	newConn, err := p.getConnection(false)
	if err != nil {
		return nil, err
	}
	// Open a new stream.
	return newConn.OpenStreamSync(ctx)
}

// openConnection opens a new QUIC connection.
func (p *dnsOverQUIC) openConnection() (conn quic.Connection, err error) {
	dialContext, err := p.getDialer()
	if err != nil {
		return nil, fmt.Errorf("failed to bootstrap QUIC connection: %w", err)
	}

	// we're using bootstrapped address instead of what's passed to the function
	// it does not create an actual connection, but it helps us determine
	// what IP is actually reachable (when there're v4/v6 addresses).
	rawConn, err := dialContext(context.Background(), "udp", "")
	if err != nil {
		return nil, fmt.Errorf("failed to open a QUIC connection: %w", err)
	}
	// It's never actually used
	_ = rawConn.Close()

	udpConn, ok := rawConn.(*net.UDPConn)
	if !ok {
		return nil, fmt.Errorf("failed to open connection to %s", p.addr)
	}

	addr := udpConn.RemoteAddr().String()

	ctx, cancel := p.withDeadline(context.Background())
	defer cancel()

	conn, err = quic.DialAddrEarlyContext(ctx, addr, p.tlsConf.Clone(), p.getQUICConfig())
	if err != nil {
		return nil, fmt.Errorf("opening quic connection to %s: %w", p.addr, err)
	}

	return conn, nil
}

// closeConnWithError closes the active connection with error to make sure that
// new queries were processed in another connection.  We can do that in the case
// of a fatal error.
func (p *dnsOverQUIC) closeConnWithError(err error) {
	p.connMu.Lock()
	defer p.connMu.Unlock()

	if p.conn == nil {
		// Do nothing, there's no active conn anyways.
		return
	}

	code := QUICCodeNoError
	if err != nil {
		code = QUICCodeInternalError
	}

	if errors.Is(err, quic.Err0RTTRejected) {
		// Reset the TokenStore only if 0-RTT was rejected.
		p.resetQUICConfig()
	}

	err = p.conn.CloseWithError(code, "")
	if err != nil {
		log.Error("failed to close the conn: %v", err)
	}
	p.conn = nil
}

// readMsg reads the incoming DNS message from the QUIC stream.
func (p *dnsOverQUIC) readMsg(stream quic.Stream) (m *dns.Msg, err error) {
	pool := p.getBytesPool()
	bufPtr := pool.Get().(*[]byte)

	defer pool.Put(bufPtr)

	respBuf := *bufPtr
	n, err := stream.Read(respBuf)
	if err != nil && n == 0 {
		return nil, fmt.Errorf("reading response from %s: %w", p.addr, err)
	}

	// All DNS messages (queries and responses) sent over DoQ connections MUST
	// be encoded as a 2-octet length field followed by the message content as
	// specified in [RFC1035].
	// IMPORTANT: Note, that we ignore this prefix here as this implementation
	// does not support receiving multiple messages over a single connection.
	m = new(dns.Msg)
	err = m.Unpack(respBuf[2:])
	if err != nil {
		return nil, fmt.Errorf("unpacking response from %s: %w", p.addr, err)
	}

	return m, nil
}

// newQUICTokenStore creates a new quic.TokenStore that is necessary to have
// in order to benefit from 0-RTT.
func newQUICTokenStore() (s quic.TokenStore) {
	// You can read more on address validation here:
	// https://datatracker.ietf.org/doc/html/rfc9000#section-8.1
	// Setting maxOrigins to 1 and tokensPerOrigin to 10 assuming that this is
	// more than enough for the way we use it (one connection per upstream).
	return quic.NewLRUTokenStore(1, 10)
}

// isQUICRetryError checks the error and determines whether it may signal that
// we should re-create the QUIC connection.  This requirement is caused by
// quic-go issues, see the comments inside this function.
// TODO(ameshkov): re-test when updating quic-go.
func isQUICRetryError(err error) (ok bool) {
	var qAppErr *quic.ApplicationError
	if errors.As(err, &qAppErr) && qAppErr.ErrorCode == 0 {
		// This error is often returned when the server has been restarted,
		// and we try to use the same connection on the client-side. It seems,
		// that the old connections aren't closed immediately on the server-side
		// and that's why one can run into this.
		// In addition to that, quic-go HTTP3 client implementation does not
		// clean up dead connections (this one is specific to DoH3 upstream):
		// https://github.com/quic-go/quic-go/issues/765
		return true
	}

	var qIdleErr *quic.IdleTimeoutError
	if errors.As(err, &qIdleErr) {
		// This error means that the connection was closed due to being idle.
		// In this case we should forcibly re-create the QUIC connection.
		// Reproducing is rather simple, stop the server and wait for 30 seconds
		// then try to send another request via the same upstream.
		return true
	}

	var resetErr *quic.StatelessResetError
	if errors.As(err, &resetErr) {
		// A stateless reset is sent when a server receives a QUIC packet that
		// it doesn't know how to decrypt.  For instance, it may happen when
		// the server was recently rebooted.  We should reconnect and try again
		// in this case.
		return true
	}

	var qTransportError *quic.TransportError
	if errors.As(err, &qTransportError) && qTransportError.ErrorCode == quic.NoError {
		// A transport error with the NO_ERROR error code could be sent by the
		// server when it considers that it's time to close the connection.
		// For example, Google DNS eventually closes an active connection with
		// the NO_ERROR code and "Connection max age expired" message:
		// https://github.com/AdguardTeam/dnsproxy/issues/283
		return true
	}

	if errors.Is(err, quic.Err0RTTRejected) {
		// This error happens when we try to establish a 0-RTT connection with
		// a token the server is no more aware of.  This can be reproduced by
		// restarting the QUIC server (it will clear its tokens cache).  The
		// next connection attempt will return this error until the client's
		// tokens cache is purged.
		return true
	}

	return false
}

func (p *dnsOverQUIC) withDeadline(
	parent context.Context,
) (ctx context.Context, cancel context.CancelFunc) {
	ctx, cancel = parent, func() {}
	if p.timeout > 0 {
		ctx, cancel = context.WithDeadline(ctx, time.Now().Add(p.timeout))
	}

	return ctx, cancel
}
