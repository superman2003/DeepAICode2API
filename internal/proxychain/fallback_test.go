package proxychain_test

import (
	"crypto/tls"
	"crypto/x509"
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

func TestDirectFallbackBeforeOriginRequestAndCircuitCooldown(t *testing.T) {
	t.Setenv(proxychain.EnvFallbackDirect, "1")
	var originCalls, proxyCalls atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originCalls.Add(1)
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Error("proxy credential leaked to origin")
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "original POST body" {
			t.Errorf("POST body was lost: %q", body)
		}
		_, _ = io.WriteString(w, "OK")
	}))
	defer origin.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls.Add(1)
		http.Error(w, "proxy unavailable", http.StatusBadGateway)
	}))
	defer proxy.Close()
	u, _ := url.Parse(proxy.URL)
	roots := x509.NewCertPool()
	roots.AddCert(origin.Certificate())
	base := &http.Transport{Proxy: http.ProxyURL(u), TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}}
	client := &http.Client{Transport: proxychain.WithDirectFallback(base, u), Timeout: 2 * time.Second}
	defer client.CloseIdleConnections()
	for attempt := 0; attempt < 2; attempt++ {
		req, _ := http.NewRequest(http.MethodPost, origin.URL, strings.NewReader("original POST body"))
		req.Header.Set("Proxy-Authorization", "must-not-reach-origin")
		response, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if string(body) != "OK" {
			t.Fatalf("direct fallback failed: %q", body)
		}
	}
	if originCalls.Load() != 2 || proxyCalls.Load() != 1 {
		t.Fatalf("circuit did not bypass broken proxy: origin=%d proxy=%d", originCalls.Load(), proxyCalls.Load())
	}
}

func TestDirectFallbackDoesNotReplayWrittenPOST(t *testing.T) {
	t.Setenv(proxychain.EnvFallbackDirect, "1")
	var originCalls atomic.Int32
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originCalls.Add(1)
		_, _ = io.ReadAll(r.Body)
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		connection.Close()
	}))
	defer origin.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { tunnel(t, w, r, origin.Listener.Addr().String()) }))
	defer proxy.Close()
	u, _ := url.Parse(proxy.URL)
	roots := x509.NewCertPool()
	roots.AddCert(origin.Certificate())
	base := &http.Transport{Proxy: http.ProxyURL(u), TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}}
	client := &http.Client{Transport: proxychain.WithDirectFallback(base, u), Timeout: 2 * time.Second}
	defer client.CloseIdleConnections()
	if _, err := client.Post(origin.URL, "text/plain", strings.NewReader("already submitted")); err == nil {
		t.Fatal("expected ambiguous origin failure")
	}
	if originCalls.Load() != 1 {
		t.Fatalf("POST was replayed: %d", originCalls.Load())
	}
}

func TestDirectFallbackIsDisabledByDefault(t *testing.T) {
	t.Setenv(proxychain.EnvFallbackDirect, "")
	base := &http.Transport{}
	u, _ := url.Parse("http://proxy.invalid:3010")
	if proxychain.WithDirectFallback(base, u) != base {
		t.Fatal("fallback changed the existing local routing without opt-in")
	}
}

func TestBothApplicationClientsEnableServerFallback(t *testing.T) {
	t.Setenv(proxychain.EnvFallbackDirect, "1")
	t.Setenv(proxychain.EnvEgressProxy, "")
	t.Setenv("DEEPAI_MACHINE_ID", "11111111-2222-3333-4444-555555555555")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxyURL := "http://" + listener.Addr().String()
	listener.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "server direct route") }))
	defer origin.Close()
	up := deepai.NewClientWithProxy(proxyURL)
	web := register.NewWebClientWithFingerprint(&register.Config{}, register.Fingerprint{}, proxyURL)
	for _, client := range []*http.Client{up.HTTP, web.HTTP} {
		client.Timeout = 2 * time.Second
		defer client.CloseIdleConnections()
		response, err := client.Get(origin.URL)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if string(body) != "server direct route" {
			t.Fatalf("application did not use direct failover: %q", body)
		}
	}
}
