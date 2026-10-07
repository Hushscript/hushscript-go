package hushscript

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// R54 over HTTP/2: the default client negotiates h2, and net/http's HTTP/2 transport sends a request
// again, rebuilding its body from GetBody, when the server resets the stream with PROTOCOL_ERROR or
// REFUSED_STREAM. The server below speaks just enough raw HTTP/2 to reset the first stream it sees after
// reading its whole body, and to answer every later stream with an empty 200.
type h2ResetServer struct {
	ln      net.Listener
	client  *http.Client
	mu      sync.Mutex
	streams int
	conns   []net.Conn
}

func newH2ResetServer(t *testing.T) *h2ResetServer {
	t.Helper()
	certSource := httptest.NewTLSServer(http.NotFoundHandler())
	certSource.Close()
	pool := x509.NewCertPool()
	pool.AddCert(certSource.Certificate())
	config := certSource.TLS.Clone()
	config.NextProtos = []string{"h2"}
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatal("http.DefaultTransport is not an *http.Transport")
	}
	cloned := transport.Clone()
	cloned.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	s := &h2ResetServer{ln: tls.NewListener(ln, config), client: &http.Client{Transport: cloned}}
	t.Cleanup(func() {
		cloned.CloseIdleConnections()
		_ = s.ln.Close()
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, conn := range s.conns {
			_ = conn.Close()
		}
	})
	go func() {
		for {
			conn, err := s.ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns = append(s.conns, conn)
			s.mu.Unlock()
			go s.serve(conn)
		}
	}()
	return s
}

func (s *h2ResetServer) URL() string { return "https://" + s.ln.Addr().String() }

// streamsSeen is how many requests reached the server, whatever their path (every test has its own server).
func (s *h2ResetServer) streamsSeen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streams
}

const (
	h2FrameData         = 0x0
	h2FrameHeaders      = 0x1
	h2FrameRSTStream    = 0x3
	h2FrameSettings     = 0x4
	h2FlagEndStream     = 0x1
	h2FlagAck           = 0x1
	h2FlagEndHeaders    = 0x4
	h2ErrorProtocol     = 0x1
	h2HPACKStatus200    = 0x88 // ":status: 200" is entry 8 of the HPACK static table
	h2ClientPrefaceSize = 24
)

func h2WriteFrame(conn net.Conn, kind, flags byte, stream uint32, payload []byte) error {
	frame := make([]byte, 9, 9+len(payload))
	frame[0], frame[1], frame[2] = byte(len(payload)>>16), byte(len(payload)>>8), byte(len(payload))
	frame[3], frame[4] = kind, flags
	binary.BigEndian.PutUint32(frame[5:], stream)
	_, err := conn.Write(append(frame, payload...))
	return err
}

func (s *h2ResetServer) serve(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	if _, err := io.ReadFull(conn, make([]byte, h2ClientPrefaceSize)); err != nil {
		return
	}
	if err := h2WriteFrame(conn, h2FrameSettings, 0, 0, nil); err != nil {
		return
	}
	for {
		header := make([]byte, 9)
		if _, err := io.ReadFull(conn, header); err != nil {
			return
		}
		length := int(header[0])<<16 | int(header[1])<<8 | int(header[2])
		kind, flags := header[3], header[4]
		stream := binary.BigEndian.Uint32(header[5:]) & 0x7fffffff
		if _, err := io.CopyN(io.Discard, conn, int64(length)); err != nil {
			return
		}
		switch {
		case kind == h2FrameSettings && flags&h2FlagAck == 0:
			if err := h2WriteFrame(conn, h2FrameSettings, h2FlagAck, 0, nil); err != nil {
				return
			}
		case (kind == h2FrameHeaders || kind == h2FrameData) && flags&h2FlagEndStream != 0:
			s.mu.Lock()
			s.streams++
			first := s.streams == 1
			s.mu.Unlock()
			var err error
			if first {
				err = h2WriteFrame(conn, h2FrameRSTStream, 0, stream, binary.BigEndian.AppendUint32(nil, h2ErrorProtocol))
			} else {
				err = h2WriteFrame(conn, h2FrameHeaders, h2FlagEndHeaders|h2FlagEndStream, stream, []byte{h2HPACKStatus200})
			}
			if err != nil {
				return
			}
		}
	}
}

func TestANonReplayableRequestIsSentOnceWhenTheServerResetsItsStreamOverHTTP2(t *testing.T) {
	const create = `{"size_bytes":100,"duration_seconds":20}`
	cases := []struct {
		name string
		kind ErrorKind
		code string
		run  func(t *testing.T, c *Client, s *h2ResetServer) error
	}{
		{
			name: "claim", kind: KindOutcomeUnknown, code: "outcome_unknown",
			run: func(_ *testing.T, _ *Client, s *h2ResetServer) error {
				_, err := AgentClaim(context.Background(), "sg_1", "cs", WithBaseURL(s.URL()), WithHTTPClient(s.client), WithClock(newFakeClock()))
				return err
			},
		},
		{
			name: "rotate", kind: KindOutcomeUnknown, code: "outcome_unknown",
			run: func(_ *testing.T, c *Client, _ *h2ResetServer) error {
				_, err := c.Agent.RotateToken(context.Background())
				return err
			},
		},
		{
			name: "purchase", kind: KindNetwork, code: "network_error",
			run: func(t *testing.T, c *Client, _ *h2ResetServer) error {
				_, err := do(t, c, http.MethodPost, "/v1/billing/purchase", `{"pack":"45min","idempotency_key":"k-0123456789abcdef"}`)
				return err
			},
		},
		{
			name: "unkeyed create", kind: KindNetwork, code: "network_error",
			run: func(t *testing.T, c *Client, _ *h2ResetServer) error {
				_, err := do(t, c, http.MethodPost, "/v1/uploads", create)
				return err
			},
		},
		{
			name: "delete without a body", kind: KindNetwork, code: "network_error",
			run: func(t *testing.T, c *Client, _ *h2ResetServer) error {
				_, err := do(t, c, http.MethodDelete, "/v1/transcripts/tr_1", "")
				return err
			},
		},
		{
			name: "org mutation", kind: KindNetwork, code: "network_error",
			run: func(t *testing.T, c *Client, _ *h2ResetServer) error {
				_, err := do(t, c, http.MethodPost, "/v1/org/custody", `{"custody":"firm"}`)
				return err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newH2ResetServer(t)
			// The purchase class retries a lost answer once on its own, with its key; switch that off so
			// only the transport could send it twice.
			c := newTestClient(t, srv.URL(), newFakeClock(), WithHTTPClient(srv.client), WithMaxRetries(0))
			err := tc.run(t, c, srv)
			e := wantCode(t, err, tc.kind, tc.code)
			if e.Status != 0 {
				t.Errorf("status %d on a request that got no answer", e.Status)
			}
			if got := srv.streamsSeen(); got != 1 {
				t.Errorf("the request reached the server %d times, want once", got)
			}
		})
	}
}

// The controls that make the test above mean something: the HTTP/2 transport does resend a request that
// is safe to repeat, so the server sees it twice and the caller never notices.
func TestARepeatableRequestIsResentByTheTransportAfterAStreamResetOverHTTP2(t *testing.T) {
	cases := map[string]func(t *testing.T, c *Client) error{
		"get": func(t *testing.T, c *Client) error {
			_, err := do(t, c, http.MethodGet, "/v1/account", "")
			return err
		},
		"keyed create": func(t *testing.T, c *Client) error {
			_, err := do(t, c, http.MethodPost, "/v1/uploads", `{"size_bytes":100,"duration_seconds":20,"idempotency_key":"k-0123456789abcdef"}`)
			return err
		},
		"part": func(t *testing.T, c *Client) error {
			_, err := doContext(withDirectStorage(context.Background(), 1), t, c, http.MethodPut, "/v1/uploads/job_1/parts/1", "bytes")
			return err
		},
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			srv := newH2ResetServer(t)
			c := newTestClient(t, srv.URL(), newFakeClock(), WithHTTPClient(srv.client), WithMaxRetries(0))
			if err := run(t, c); err != nil {
				t.Fatalf("the transport's replay should have succeeded: %v", err)
			}
			if got := srv.streamsSeen(); got != 2 {
				t.Errorf("the request reached the server %d times, want the original and one replay", got)
			}
		})
	}
}

// Whatever the server does, the request the SDK hands to the transport carries no GetBody for the
// classes that must never be sent twice, and keeps it for the classes that may.
func TestOnlyReplayableClassesHandTheTransportAGetBody(t *testing.T) {
	cases := []struct {
		name, method, path, body string
		replayable               bool
	}{
		{"claim", http.MethodPost, "/v1/agent/accounts/sg_1/claim", `{"claim_secret":"cs"}`, false},
		{"rotate", http.MethodPost, "/v1/agent/credentials/rotate", `{}`, false},
		{"signup", http.MethodPost, "/v1/agent/accounts", `{}`, false},
		{"purchase", http.MethodPost, "/v1/billing/purchase", `{"pack":"45min"}`, false},
		{"setup session", http.MethodPost, "/v1/billing/setup-session", `{}`, false},
		{"mutation", http.MethodPatch, "/v1/org/members/u_1", `{"role":"admin"}`, false},
		{"unkeyed create", http.MethodPost, "/v1/uploads", `{"size_bytes":100}`, false},
		{"keyed create", http.MethodPost, "/v1/uploads", `{"size_bytes":100,"idempotency_key":"k-0123456789abcdef"}`, true},
		{"complete", http.MethodPost, "/v1/uploads/job_1/complete", `{}`, true},
		{"part", http.MethodPut, "/v1/uploads/job_1/parts/1", `bytes`, true},
		{"heartbeat", http.MethodPost, "/v1/uploads/job_1/heartbeat", `{}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hadGetBody bool
			transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
				hadGetBody = req.GetBody != nil
				return &http.Response{
					StatusCode: http.StatusOK, Status: "200 OK", Header: http.Header{"Content-Type": {"application/json"}}, Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
					Body: io.NopCloser(http.NoBody), Request: req,
				}, nil
			})
			c := newTestClient(t, "http://127.0.0.1:1", newFakeClock(), WithHTTPClient(&http.Client{Transport: transport}))
			if _, err := do(t, c, tc.method, tc.path, tc.body); err != nil {
				t.Fatal(err)
			}
			if hadGetBody != tc.replayable {
				t.Errorf("GetBody present = %v, want %v", hadGetBody, tc.replayable)
			}
		})
	}
}
