// Package probe performs bounded background probes entirely inside the plugin.
// It uses only public Go libraries and existing CPA read-only callbacks.
package probe

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"

	utls "github.com/refraction-networking/utls"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	"golang.org/x/net/http2"
	"golang.org/x/net/proxy"
)

// One HTTP/2 connection per probe, owned by its response body. There is no
// shared business transport, default/environment proxy or direct fallback.
type chromeTransport struct{ dialer proxy.ContextDialer }

func newTransport(route string) (http.RoundTripper, error) {
	dialer, _, err := proxyutil.BuildDialer(route)
	if err != nil {
		return nil, errors.New("invalid probe proxy")
	}
	if dialer == nil {
		return nil, errors.New("probe route must be explicit")
	}
	ctxDialer, ok := dialer.(proxy.ContextDialer)
	if !ok {
		return nil, errors.New("proxy lacks context dialing")
	}
	return &chromeTransport{ctxDialer}, nil
}
func (t *chromeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" || req.URL.Hostname() != "chatgpt.com" || req.URL.Port() != "" {
		return nil, errors.New("probe endpoint not allowed")
	}
	// Pass the hostname, not a locally resolved IP, through the SOCKS dialer.
	conn, err := t.dialer.DialContext(req.Context(), "tcp", net.JoinHostPort(req.URL.Hostname(), "443"))
	if err != nil {
		return nil, errors.New("probe proxy dial failed")
	}
	tlsConn := utls.UClient(conn, &utls.Config{ServerName: req.URL.Hostname()}, utls.HelloChrome_Auto)
	if err = tlsConn.HandshakeContext(req.Context()); err != nil {
		conn.Close()
		return nil, errors.New("probe TLS handshake failed")
	}
	if tlsConn.ConnectionState().NegotiatedProtocol != "h2" {
		tlsConn.Close()
		return nil, errors.New("probe requires HTTP/2")
	}
	transport := &http2.Transport{}
	h2, err := transport.NewClientConn(tlsConn)
	if err != nil {
		tlsConn.Close()
		return nil, errors.New("probe HTTP/2 setup failed")
	}
	response, err := h2.RoundTrip(req)
	if err != nil {
		h2.Close()
		return nil, errors.New("probe HTTP/2 request failed")
	}
	if response == nil || response.Body == nil {
		h2.Close()
		return nil, errors.New("empty probe response")
	}
	body := &ownedBody{ReadCloser: response.Body, closeConn: h2.Close}
	body.stop = context.AfterFunc(req.Context(), func() { _ = h2.Close() })
	response.Body = body
	return response, nil
}

type ownedBody struct {
	io.ReadCloser
	once      sync.Once
	closeConn func() error
	stop      func() bool
	err       error
}

func (b *ownedBody) Close() error {
	b.once.Do(func() {
		if b.stop != nil {
			b.stop()
		}
		b.err = b.ReadCloser.Close()
		if err := b.closeConn(); b.err == nil {
			b.err = err
		}
	})
	return b.err
}
