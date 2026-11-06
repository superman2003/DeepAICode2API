package proxychain

import (
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

const EnvFallbackDirect = "DEEPAI2API_PROXY_FALLBACK_DIRECT"

// WithDirectFallback is opt-in. A failed proxy connection may be retried over
// direct egress only before the origin request has been written. In particular,
// an ambiguous POST failure is never replayed by this transport.
func WithDirectFallback(proxy *http.Transport, explicit *url.URL) http.RoundTripper {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(EnvFallbackDirect)))
	if explicit == nil || (value != "1" && value != "true" && value != "yes") {
		return proxy
	}
	if proxy.DialContext == nil {
		proxy.DialContext = (&net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	}
	direct := proxy.Clone()
	direct.Proxy = nil
	direct.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	direct.DialTLSContext = nil
	direct.DialTLS = nil
	return &directFallback{proxy: proxy, direct: direct}
}

type directFallback struct {
	proxy, direct *http.Transport
	openUntil     atomic.Int64
}

func (f *directFallback) RoundTrip(req *http.Request) (*http.Response, error) {
	if time.Now().UnixNano() < f.openUntil.Load() {
		direct := req.Clone(req.Context())
		direct.Header.Del("Proxy-Authorization")
		return f.direct.RoundTrip(direct)
	}
	var wrote atomic.Bool
	traced := req.Clone(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{WroteRequest: func(httptrace.WroteRequestInfo) { wrote.Store(true) }}))
	response, err := f.proxy.RoundTrip(traced)
	if err == nil {
		f.openUntil.Store(0)
		return response, nil
	}
	if req.Context().Err() != nil || wrote.Load() {
		return nil, err
	}
	// A body may have been closed even when the connection never reached the
	// origin. Only retry when a fresh copy of that body is available.
	if req.Body != nil && req.Body != http.NoBody && req.GetBody == nil {
		return nil, err
	}
	f.openUntil.Store(time.Now().Add(time.Minute).UnixNano())
	retry := req.Clone(req.Context())
	retry.Header.Del("Proxy-Authorization")
	if req.GetBody != nil {
		body, bodyErr := req.GetBody()
		if bodyErr != nil {
			return nil, bodyErr
		}
		retry.Body = body
	}
	return f.direct.RoundTrip(retry)
}

func (f *directFallback) CloseIdleConnections() {
	f.proxy.CloseIdleConnections()
	f.direct.CloseIdleConnections()
}

var _ http.RoundTripper = (*directFallback)(nil)
