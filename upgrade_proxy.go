package main

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"

	apimachineryproxy "k8s.io/apimachinery/pkg/util/proxy"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport"
	"k8s.io/klog/v2"
)

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
	if len(tlsConfig.NextProtos) == 0 {
		tlsConfig.NextProtos = []string{"http/1.1"}
	}

	authWrappers, err := transport.HTTPWrappersForConfig(transportConfig, apimachineryproxy.MirrorRequest)
	if err != nil {
		return nil, fmt.Errorf("auth wrappers: %w", err)
	}

	return &upgradePipeHandler{
		apiPrefix:    apiPrefix,
		target:       target,
		tlsConfig:    tlsConfig,
		authWrappers: authWrappers,
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

	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, "", nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	outReq.URL = &url.URL{
		Scheme:   h.target.Scheme,
		Host:     h.target.Host,
		Path:     targetPath,
		RawQuery: r.URL.RawQuery,
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
	authedReq := resp.Request

	addr := h.target.Host
	if !strings.Contains(addr, ":") {
		addr += ":443"
	}

	tlsCfg := h.tlsConfig.Clone()
	if tlsCfg.ServerName == "" {
		host, _, splitErr := net.SplitHostPort(addr)
		if splitErr != nil {
			host = addr
		}
		tlsCfg.ServerName = host
	}

	upstreamConn, err := tls.Dial("tcp", addr, tlsCfg)
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
		errCh <- err
	}()
	go func() {
		_, err := io.Copy(clientConn, upstreamConn)
		errCh <- err
	}()
	<-errCh
}
