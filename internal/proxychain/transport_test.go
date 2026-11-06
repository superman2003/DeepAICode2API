package proxychain_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"deepaicode2api/internal/deepai"
	"deepaicode2api/internal/proxychain"
	"deepaicode2api/internal/register"
)

func basic(user, pass string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+pass))
}

func tunnel(t *testing.T, w http.ResponseWriter, r *http.Request, destination string) {
	t.Helper()
	remote, err := net.DialTimeout("tcp", destination, time.Second)
	if err != nil {
		t.Error(err)
		http.Error(w, "unreachable", http.StatusBadGateway)
		return
	}
	conn, rw, err := w.(http.Hijacker).Hijack()
	if err != nil {
		_ = remote.Close()
		t.Error(err)
		return
	}
	defer conn.Close()
	defer remote.Close()
	_, _ = rw.WriteString("HTTP/1.1 200 Connection established\r\n\r\n")
	_ = rw.Flush()
	go func() { _, _ = io.Copy(remote, rw) }()
	_, _ = io.Copy(conn, remote)
}

// Both real application clients must use the complete two-proxy chain. A fake
// remote hostname deliberately cannot resolve without the egress CONNECT hop.
func TestRegistrationAndInferenceUseEgressThenRemoteProxy(t *testing.T) {
	t.Setenv("DEEPAI_MACHINE_ID", "11111111-2222-3333-4444-555555555555")
	var originCalls, remoteCalls, egressCalls atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originCalls.Add(1)
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Error("proxy credentials leaked to origin")
		}
		_, _ = io.WriteString(w, "chained")
	}))
	t.Cleanup(origin.Close)
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remoteCalls.Add(1)
		if r.Method != http.MethodConnect || r.Header.Get("Proxy-Authorization") != basic("remote-user", "remote-secret") {
			t.Error("remote proxy did not receive its own CONNECT authentication")
			http.Error(w, "unauthorized", http.StatusProxyAuthRequired)
			return
		}
		tunnel(t, w, r, origin.Listener.Addr().String())
	}))
	t.Cleanup(remote.Close)
	egress := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		egressCalls.Add(1)
		if r.Method != http.MethodConnect || r.Host != "remote.proxy.invalid:3010" || r.Header.Get("Proxy-Authorization") != basic("egress-user", "egress-secret") {
			t.Error("egress must connect to remote proxy with separate credentials")
			http.Error(w, "wrong hop", http.StatusBadGateway)
			return
		}
		tunnel(t, w, r, remote.Listener.Addr().String())
	}))
	t.Cleanup(egress.Close)
	egressURL, _ := url.Parse(egress.URL)
	egressURL.User = url.UserPassword("egress-user", "egress-secret")
	t.Setenv(proxychain.EnvEgressProxy, egressURL.String())
	const remoteURL = "http://remote-user:remote-secret@remote.proxy.invalid:3010"
	roots := x509.NewCertPool()
	roots.AddCert(origin.Certificate())
	web := register.NewWebClientWithFingerprint(&register.Config{}, register.Fingerprint{}, remoteURL)
	up := deepai.NewClientWithProxy(remoteURL)
	for _, appClient := range []*http.Client{web.HTTP, up.HTTP} {
		transport := appClient.Transport.(*http.Transport)
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
		t.Cleanup(transport.CloseIdleConnections)
		appClient.Timeout = 3 * time.Second
		response, err := appClient.Get(origin.URL)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if string(body) != "chained" {
			t.Fatalf("wrong chained response: %q", body)
		}
	}
	if originCalls.Load() != 2 || remoteCalls.Load() != 2 || egressCalls.Load() != 2 {
		t.Fatalf("incomplete chain: origin=%d remote=%d egress=%d", originCalls.Load(), remoteCalls.Load(), egressCalls.Load())
	}
}

func TestEgressFailureDoesNotFallBackToDirectProxy(t *testing.T) {
	var remoteCalls atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		remoteCalls.Add(1)
		_, _ = io.WriteString(w, "unexpected direct connection")
	}))
	defer remote.Close()
	egress := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "blocked", http.StatusBadGateway)
	}))
	defer egress.Close()
	t.Setenv(proxychain.EnvEgressProxy, egress.URL)
	proxy, _ := url.Parse(remote.URL)
	transport := &http.Transport{Proxy: http.ProxyURL(proxy)}
	defer transport.CloseIdleConnections()
	proxychain.Configure(transport, proxy)
	client := &http.Client{Transport: transport, Timeout: time.Second}
	_, err := client.Get("http://origin.invalid/")
	if err == nil || !strings.Contains(err.Error(), "HTTP 502") || remoteCalls.Load() != 0 {
		t.Fatalf("egress failure bypassed the chain: err=%v remoteCalls=%d", err, remoteCalls.Load())
	}
}

func TestEgressConnectRespectsCancellation(t *testing.T) {
	release := make(chan struct{})
	egress := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer egress.Close()
	defer close(release)
	t.Setenv(proxychain.EnvEgressProxy, egress.URL)
	proxy, _ := url.Parse("http://remote.invalid:3010")
	transport := &http.Transport{Proxy: http.ProxyURL(proxy)}
	defer transport.CloseIdleConnections()
	proxychain.Configure(transport, proxy)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://origin.invalid/", nil)
	_, err := (&http.Client{Transport: transport}).Do(request)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CONNECT ignored cancellation: %v", err)
	}
}

func TestLocalProxyDoesNotTunnelThroughItself(t *testing.T) {
	var calls atomic.Int32
	egress := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method == http.MethodConnect {
			t.Error("attempted proxy loop")
			http.Error(w, "loop", 500)
			return
		}
		_, _ = io.WriteString(w, "local proxy")
	}))
	defer egress.Close()
	t.Setenv(proxychain.EnvEgressProxy, egress.URL)
	proxy, _ := url.Parse(egress.URL)
	transport := &http.Transport{Proxy: http.ProxyURL(proxy)}
	defer transport.CloseIdleConnections()
	proxychain.Configure(transport, proxy)
	response, err := (&http.Client{Transport: transport, Timeout: time.Second}).Get("http://origin.invalid/")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if calls.Load() != 1 {
		t.Fatalf("unexpected requests: %d", calls.Load())
	}
}
