// Package proxychain connects an explicit account proxy through a local egress
// proxy while leaving the account proxy, its credentials and its exit IP intact.
package proxychain

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const EnvEgressProxy = "DEEPAI2API_EGRESS_PROXY"

// Configure only changes the TCP connection to an explicit proxy. Transport
// still performs that proxy's HTTP CONNECT, TLS or SOCKS authentication itself.
// Requests without an explicit proxy retain http.ProxyFromEnvironment behavior.
func Configure(transport *http.Transport, explicit *url.URL) {
	raw := strings.TrimSpace(os.Getenv(EnvEgressProxy))
	if raw == "" || explicit == nil {
		return
	}
	egress, err := url.Parse(raw)
	if err != nil || egress.Hostname() == "" || (egress.Scheme != "http" && egress.Scheme != "https") {
		transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New(EnvEgressProxy + " 必须是有效的 http:// 或 https:// 代理地址")
		}
		return
	}
	if proxyAddress(explicit) == proxyAddress(egress) {
		return // Do not tunnel a local proxy through itself.
	}
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return dialThroughEgress(ctx, network, address, egress)
	}
}

func proxyAddress(proxy *url.URL) string {
	port := proxy.Port()
	if port == "" {
		switch proxy.Scheme {
		case "https":
			port = "443"
		case "socks5", "socks5h":
			port = "1080"
		default:
			port = "80"
		}
	}
	return net.JoinHostPort(strings.ToLower(proxy.Hostname()), port)
}

func dialThroughEgress(ctx context.Context, network, address string, egress *url.URL) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, errors.New("代理出口只支持 TCP 连接")
	}
	connectCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	var conn net.Conn
	var err error
	if egress.Scheme == "https" {
		tlsDialer := &tls.Dialer{NetDialer: dialer, Config: &tls.Config{
			MinVersion: tls.VersionTLS12, ServerName: egress.Hostname(),
		}}
		conn, err = tlsDialer.DialContext(connectCtx, network, proxyAddress(egress))
	} else {
		conn, err = dialer.DialContext(connectCtx, network, proxyAddress(egress))
	}
	if err != nil {
		return nil, fmt.Errorf("连接代理出口 %s 失败: %w", egress.Host, err)
	}
	stop := context.AfterFunc(connectCtx, func() { _ = conn.Close() })
	defer stop()
	success := false
	defer func() {
		if !success {
			_ = conn.Close()
		}
	}()
	request := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: address}, Host: address,
		Header: make(http.Header),
	}
	if egress.User != nil {
		password, _ := egress.User.Password()
		token := base64.StdEncoding.EncodeToString([]byte(egress.User.Username() + ":" + password))
		request.Header.Set("Proxy-Authorization", "Basic "+token)
	}
	if err := request.Write(conn); err != nil {
		return nil, fmt.Errorf("向代理出口发送 CONNECT 失败: %w", err)
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		return nil, fmt.Errorf("读取代理出口 CONNECT 响应失败: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("代理出口连接远程代理 %s 失败: HTTP %d", address, response.StatusCode)
	}
	if !stop() || connectCtx.Err() != nil {
		return nil, connectCtx.Err()
	}
	success = true
	if reader.Buffered() > 0 {
		return &bufferedConn{Conn: conn, reader: reader}, nil
	}
	return conn, nil
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (conn *bufferedConn) Read(p []byte) (int, error) { return conn.reader.Read(p) }
