package main

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	apimachineryproxy "k8s.io/apimachinery/pkg/util/proxy"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport"
	"k8s.io/klog/v2"
)

const upstreamDialTimeout = 30 * time.Second

// upgradePipeHandler proxies HTTP upgrade requests (WebSocket, SPDY) by opening
// a raw TLS connection to the upstream and piping bytes bidirectionally.
//
// Bypasses apimachinery's UpgradeAwareHandler, which mishandles streaming
// upgrades when forwarded through a bastion/proxy that expects specific
// upgrade negotiation (Warpgate, Teleport-style upstreams). The previous
// behavior caused kubectl exec/port-forward to receive a 404 page not found
// from the proxy chain even though the upstream itself accepts the upgrade
// when contacted directly.
type upgradePipeHandler struct {
	apiPrefix    string
	target       *url.URL
	tlsConfig    *tls.Config
	authWrappers http.RoundTripper
	proxyFunc    func(*http.Request) (*url.URL, error)
}

func newUpgradePipeHandler(apiPrefix string, cfg *rest.Config) (*upgradePipeHandler, error) {
	target, err := url.Parse(cfg.Host)
	if err != nil {
		return nil, fmt.Errorf("parse target host: %w", err)
	}
	if !strings.HasSuffix(target.Path, "/") {
		target.Path += "/"
	}

	transportConfig, err := cfg.TransportConfig()
	if err != nil {
		return nil, fmt.Errorf("transport config: %w", err)
	}
	tlsConfig, err := transport.TLSConfigFor(transportConfig)
	if err != nil {
		return nil, fmt.Errorf("tls config: %w", err)
	}
	if tlsConfig == nil {
		tlsConfig = &tls.Config{}
	}
	// Force HTTP/1.1 ALPN: HTTP/2 cannot carry the SPDY/WebSocket upgrade
	// negotiation we pipe through this handler.
	tlsConfig.NextProtos = []string{"http/1.1"}

	authWrappers, err := transport.HTTPWrappersForConfig(transportConfig, apimachineryproxy.MirrorRequest)
	if err != nil {
		return nil, fmt.Errorf("auth wrappers: %w", err)
	}

	proxyFunc := transportConfig.Proxy
	if proxyFunc == nil {
		proxyFunc = http.ProxyFromEnvironment
	}

	return &upgradePipeHandler{
		apiPrefix:    apiPrefix,
		target:       target,
		tlsConfig:    tlsConfig,
		authWrappers: authWrappers,
		proxyFunc:    proxyFunc,
	}, nil
}

func isUpgradeRequest(r *http.Request) bool {
	for _, h := range r.Header.Values("Connection") {
		for _, t := range strings.Split(h, ",") {
			if strings.EqualFold(strings.TrimSpace(t), "upgrade") {
				return true
			}
		}
	}
	return false
}

func singleJoiningSlash(a, b string) string {
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}

func (h *upgradePipeHandler) dialUpstream(ctx context.Context, req *http.Request, addr, sni string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: upstreamDialTimeout}

	proxyURL, err := h.proxyFunc(req)
	if err != nil {
		return nil, fmt.Errorf("resolve proxy: %w", err)
	}

	var rawConn net.Conn
	if proxyURL != nil {
		rawConn, err = dialThroughProxy(ctx, dialer, proxyURL, addr)
	} else {
		rawConn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return nil, err
	}

	if h.target.Scheme != "https" {
		return rawConn, nil
	}

	tlsCfg := h.tlsConfig.Clone()
	if tlsCfg.ServerName == "" {
		tlsCfg.ServerName = sni
	}
	tlsConn := tls.Client(rawConn, tlsCfg)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = rawConn.Close()
		return nil, err
	}
	return tlsConn, nil
}

// dialThroughProxy opens a TCP connection to the HTTP proxy and issues a
// CONNECT request so subsequent bytes (TLS handshake or plain HTTP) reach
// the upstream transparently.
func dialThroughProxy(ctx context.Context, dialer *net.Dialer, proxyURL *url.URL, targetAddr string) (net.Conn, error) {
	proxyHost := proxyURL.Hostname()
	proxyPort := proxyURL.Port()
	if proxyPort == "" {
		if proxyURL.Scheme == "https" {
			proxyPort = "443"
		} else {
			proxyPort = "80"
		}
	}
	proxyAddr := net.JoinHostPort(proxyHost, proxyPort)

	conn, err := dialer.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("dial proxy %s: %w", proxyAddr, err)
	}

	if proxyURL.Scheme == "https" {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: proxyHost})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("tls handshake to proxy %s: %w", proxyAddr, err)
		}
		conn = tlsConn
	}

	// Bound the CONNECT handshake: SetDeadline guards against a proxy that
	// accepts the TCP connection but stalls before replying, and the watcher
	// goroutine aborts the blocking Write/ReadResponse if the request context
	// is canceled. The deadline is cleared before the conn is handed off.
	if err := conn.SetDeadline(time.Now().Add(upstreamDialTimeout)); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("set deadline on proxy %s: %w", proxyAddr, err)
	}
	handshakeDone := make(chan struct{})
	defer close(handshakeDone)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetDeadline(time.Now())
		case <-handshakeDone:
		}
	}()

	connectReq := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: targetAddr},
		Host:   targetAddr,
		Header: make(http.Header),
	}
	if u := proxyURL.User; u != nil {
		username := u.Username()
		password, _ := u.Password()
		creds := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
		connectReq.Header.Set("Proxy-Authorization", "Basic "+creds)
	}
	if err := connectReq.Write(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("write CONNECT to %s: %w", proxyAddr, err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, connectReq)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read CONNECT response from %s: %w", proxyAddr, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("proxy CONNECT %s: %s", proxyAddr, resp.Status)
	}

	// Clear the handshake deadline so it does not affect the piped stream.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("clear deadline on proxy %s: %w", proxyAddr, err)
	}

	if br.Buffered() == 0 {
		return conn, nil
	}
	return &bufferedConn{Conn: conn, r: br}, nil
}

// bufferedConn glues a bufio.Reader to a net.Conn so leftover bytes parsed
// by http.ReadResponse during the CONNECT handshake are not lost.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) {
	return c.r.Read(p)
}

func (c *bufferedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

func (h *upgradePipeHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Strip the apiPrefix from the request path, mirroring stripLeaveSlash
	stripped := strings.TrimPrefix(r.URL.Path, h.apiPrefix)
	if len(stripped) >= len(r.URL.Path) {
		http.NotFound(w, r)
		return
	}
	if len(stripped) > 0 && stripped[:1] != "/" {
		stripped = "/" + stripped
	}

	targetPath := singleJoiningSlash(h.target.Path, stripped)

	targetURL := &url.URL{
		Scheme:   h.target.Scheme,
		Host:     h.target.Host,
		Path:     targetPath,
		RawQuery: r.URL.RawQuery,
	}
	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL.String(), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	outReq.Host = h.target.Host
	outReq.Header = r.Header.Clone()
	outReq.Header.Del("Authorization") // ensure HTTPWrappersForConfig adds the kubeconfig auth

	// Run through the auth wrapper chain (BasicAuth/Token/etc.) ending at
	// MirrorRequest, which captures the now-authenticated request and returns
	// it back via response.Request.
	resp, err := h.authWrappers.RoundTrip(outReq)
	if err != nil {
		http.Error(w, "auth wrap: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	authedReq := resp.Request

	host := h.target.Hostname()
	port := h.target.Port()
	if port == "" {
		if h.target.Scheme == "http" {
			port = "80"
		} else {
			port = "443"
		}
	}
	addr := net.JoinHostPort(host, port)

	upstreamConn, err := h.dialUpstream(r.Context(), authedReq, addr, host)
	if err != nil {
		klog.Errorf("[UPGRADE-PIPE] dial %s failed: %v", addr, err)
		http.Error(w, "dial upstream: "+err.Error(), http.StatusBadGateway)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		_ = upstreamConn.Close()
		http.Error(w, "response writer is not hijackable", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		_ = upstreamConn.Close()
		klog.Errorf("[UPGRADE-PIPE] hijack failed: %v", err)
		return
	}
	defer func() { _ = clientConn.Close() }()
	defer func() { _ = upstreamConn.Close() }()

	klog.V(2).Infof("[UPGRADE-PIPE] %s %s -> %s", r.Method, r.URL.Path, authedReq.URL.String())

	if err := authedReq.Write(upstreamConn); err != nil {
		klog.Errorf("[UPGRADE-PIPE] write upstream: %v", err)
		return
	}

	errCh := make(chan error, 2)
	go func() {
		_, err := io.Copy(upstreamConn, clientConn)
		closeWrite(upstreamConn)
		errCh <- err
	}()
	go func() {
		_, err := io.Copy(clientConn, upstreamConn)
		closeWrite(clientConn)
		errCh <- err
	}()
	<-errCh
	<-errCh
}

// closeWrite signals end-of-stream on the write side of conn if the
// underlying type supports it, so the peer sees a clean FIN/close_notify
// rather than an abrupt reset when one direction finishes first.
func closeWrite(conn net.Conn) {
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}
