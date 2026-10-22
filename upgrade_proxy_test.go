package main

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// upgradeUpstream stands in for the API server for upgrade requests. It
// answers one with 101 Switching Protocols and then echoes what it reads,
// until the other side closes its write side. It offers HTTP/2 too, so that
// the test sees the pipe ask for HTTP/1.1.
type upgradeUpstream struct {
	*httptest.Server

	mu            sync.Mutex
	path          string
	rawQuery      string
	authorization string
	protocol      string
}

func newUpgradeUpstream(t *testing.T) *upgradeUpstream {
	t.Helper()

	s := &upgradeUpstream{}
	s.Server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.path = r.URL.Path
		s.rawQuery = r.URL.RawQuery
		s.authorization = r.Header.Get("Authorization")
		s.protocol = r.TLS.NegotiatedProtocol
		s.mu.Unlock()

		hijacker, ok := w.(http.Hijacker)

		if !isUpgradeRequest(r) || !ok {
			http.Error(w, "not an upgrade", http.StatusBadRequest)
			return
		}

		conn, rw, err := hijacker.Hijack()

		if err != nil {
			return
		}

		defer func() { _ = conn.Close() }()

		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n")
		_ = rw.Flush()
		_, _ = io.Copy(conn, rw.Reader)
	}))
	s.EnableHTTP2 = true
	// Without NextProtos, httptest offers h2 alone once HTTP/2 is on.
	s.TLS = &tls.Config{NextProtos: []string{"h2", "http/1.1"}}
	s.StartTLS()
	t.Cleanup(s.Close)

	return s
}

func (s *upgradeUpstream) lastRequest() (path, rawQuery, authorization, protocol string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.path, s.rawQuery, s.authorization, s.protocol
}

func caData(s *httptest.Server) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.Certificate().Raw})
}

// writeClusterKubeconfig writes a kubeconfig with the cluster and a user
// with the test token.
func writeClusterKubeconfig(t *testing.T, cluster *clientcmdapi.Cluster) string {
	t.Helper()

	config := clientcmdapi.NewConfig()
	config.Clusters["a"] = cluster
	config.AuthInfos["user"] = &clientcmdapi.AuthInfo{Token: testBearerToken}
	config.Contexts["a"] = &clientcmdapi.Context{Cluster: "a", AuthInfo: "user"}
	config.CurrentContext = "a"

	path := filepath.Join(t.TempDir(), "kubeconfig")

	if err := clientcmd.WriteToFile(*config, path); err != nil {
		t.Fatalf("failed to write kubeconfig: %v", err)
	}

	return path
}

// upgrade sends an upgrade request through the proxy, with a token of its
// own, and returns the connection and the response.
func upgrade(t *testing.T, addr, path string) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()

	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)

	if err != nil {
		t.Fatalf("failed to connect to the proxy: %v", err)
	}

	t.Cleanup(func() { _ = conn.Close() })

	// Nothing in a test may hang for the timeouts of the proxy.
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	_, err = fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: test\r\nAuthorization: Bearer client-token\r\n\r\n", path, addr)

	if err != nil {
		t.Fatalf("failed to send the upgrade request: %v", err)
	}

	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)

	if err != nil {
		t.Fatalf("failed to read the response: %v", err)
	}

	return conn, br, resp
}

// expectEcho checks that bytes go both ways, and that closing the write side
// of the client ends the stream instead of leaving it open.
func expectEcho(t *testing.T, conn net.Conn, br *bufio.Reader) {
	t.Helper()

	if _, err := io.WriteString(conn, "ping"); err != nil {
		t.Fatalf("failed to write: %v", err)
	}

	got := make([]byte, 4)

	if _, err := io.ReadFull(br, got); err != nil || string(got) != "ping" {
		t.Fatalf("read %q, %v, want the echo of \"ping\"", got, err)
	}

	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("failed to close the write side: %v", err)
	}

	rest, err := io.ReadAll(br)

	if err != nil {
		t.Fatalf("the stream did not end after the write side was closed: %v", err)
	}

	if len(rest) != 0 {
		t.Errorf("read %q after the echo, want nothing", rest)
	}
}

func TestUpgradePipe(t *testing.T) {
	upstream := newUpgradeUpstream(t)

	tests := []struct {
		name       string
		serverPath string
		apiPrefix  string
		path       string
		want       string
	}{
		{
			name: "root prefix",
			path: "/api/v1/namespaces/default/pods/p/exec",
			want: "/api/v1/namespaces/default/pods/p/exec",
		},
		{
			name:      "API prefix",
			apiPrefix: "foo",
			path:      "/foo/api/v1/namespaces/default/pods/p/exec",
			want:      "/api/v1/namespaces/default/pods/p/exec",
		},
		{
			// A Rancher kubeconfig names a path on the server.
			name:       "server path",
			serverPath: "/k8s/clusters/c-1",
			apiPrefix:  "foo",
			path:       "/foo/api/v1/namespaces/default/pods/p/exec",
			want:       "/k8s/clusters/c-1/api/v1/namespaces/default/pods/p/exec",
		},
		{
			name:       "server path with a slash",
			serverPath: "/k8s/clusters/c-1/",
			path:       "/api/v1/namespaces/default/pods/p/exec",
			want:       "/k8s/clusters/c-1/api/v1/namespaces/default/pods/p/exec",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kubeconfig := writeClusterKubeconfig(t, &clientcmdapi.Cluster{
				Server:                   upstream.URL + tt.serverPath,
				CertificateAuthorityData: caData(upstream.Server),
			})

			p := startProxy(t, map[string]string{
				"KUBECONFIG":                kubeconfig,
				"FREELENS_PROXY_API_PREFIX": tt.apiPrefix,
			})

			conn, br, resp := upgrade(t, p.addr, tt.path+"?command=sh&stdin=true")

			if resp.StatusCode != http.StatusSwitchingProtocols {
				t.Fatalf("status = %d, want 101", resp.StatusCode)
			}

			expectEcho(t, conn, br)

			path, rawQuery, authorization, protocol := upstream.lastRequest()

			if path != tt.want {
				t.Errorf("the API server got the path %q, want %q", path, tt.want)
			}

			if rawQuery != "command=sh&stdin=true" {
				t.Errorf("the API server got the query %q, want it unchanged", rawQuery)
			}

			// The token of the client is replaced, so a client of the proxy
			// cannot pass a credential of its own.
			if authorization != "Bearer "+testBearerToken {
				t.Errorf("the API server got Authorization %q, want the token of the kubeconfig", authorization)
			}

			// HTTP/2 cannot carry the upgrade.
			if protocol != "http/1.1" {
				t.Errorf("negotiated protocol = %q, want http/1.1", protocol)
			}
		})
	}
}

func TestUpgradeOutsidePrefix(t *testing.T) {
	upstream := newUpgradeUpstream(t)
	kubeconfig := writeClusterKubeconfig(t, &clientcmdapi.Cluster{
		Server:                   upstream.URL,
		CertificateAuthorityData: caData(upstream.Server),
	})

	p := startProxy(t, map[string]string{
		"KUBECONFIG":                kubeconfig,
		"FREELENS_PROXY_API_PREFIX": "foo",
	})

	_, _, resp := upgrade(t, p.addr, "/api/v1/namespaces/default/pods/p/exec")

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestUpgradeUntrustedUpstream(t *testing.T) {
	upstream := newUpgradeUpstream(t)

	// Another CA does not verify the upstream. It cannot be the CA of another
	// httptest server: they all share one certificate.
	otherCA, _, _ := selfSignedCert(t)

	kubeconfig := writeClusterKubeconfig(t, &clientcmdapi.Cluster{
		Server:                   upstream.URL,
		CertificateAuthorityData: []byte(otherCA),
	})

	p := startProxy(t, map[string]string{"KUBECONFIG": kubeconfig})

	_, _, resp := upgrade(t, p.addr, "/api/v1/namespaces/default/pods/p/exec")

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}

	if path, _, _, _ := upstream.lastRequest(); path != "" {
		t.Errorf("the API server got a request for %q through an unverified connection", path)
	}
}

func TestIsUpgradeRequest(t *testing.T) {
	tests := []struct {
		connection []string
		want       bool
	}{
		{connection: nil, want: false},
		{connection: []string{"Upgrade"}, want: true},
		{connection: []string{"upgrade"}, want: true},
		{connection: []string{"keep-alive, Upgrade"}, want: true},
		{connection: []string{"keep-alive", "Upgrade"}, want: true},
		{connection: []string{"keep-alive"}, want: false},
		{connection: []string{"close"}, want: false},
		{connection: []string{"Upgrades"}, want: false},
	}

	for _, tt := range tests {
		t.Run(strings.Join(tt.connection, "|"), func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/", nil)

			for _, value := range tt.connection {
				r.Header.Add("Connection", value)
			}

			if got := isUpgradeRequest(r); got != tt.want {
				t.Errorf("isUpgradeRequest = %v, want %v", got, tt.want)
			}
		})
	}
}

// connectProxy is an HTTP proxy that answers CONNECT. By default it tunnels
// to the target. With a status other than 200 it refuses, with early it
// sends those bytes right after the 200 and then plays the upstream itself,
// and with stall it never answers.
type connectProxy struct {
	listener net.Listener

	status int
	early  string
	stall  bool

	mu            sync.Mutex
	target        string
	authorization string

	connected chan struct{}
	closed    chan struct{}
}

func newConnectProxy(t *testing.T, configure func(*connectProxy)) *connectProxy {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")

	if err != nil {
		t.Fatalf("failed to listen: %v", err)
	}

	p := &connectProxy{
		listener:  l,
		status:    http.StatusOK,
		connected: make(chan struct{}, 1),
		closed:    make(chan struct{}, 1),
	}

	if configure != nil {
		configure(p)
	}

	t.Cleanup(func() { _ = l.Close() })

	go func() {
		for {
			conn, err := l.Accept()

			if err != nil {
				return
			}

			go p.handle(conn)
		}
	}()

	return p
}

func (p *connectProxy) url(userinfo string) string {
	return "http://" + userinfo + p.listener.Addr().String()
}

func (p *connectProxy) lastConnect() (string, string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.target, p.authorization
}

// notify signals the channel without waiting for a reader.
func notify(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

func (p *connectProxy) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	br := bufio.NewReader(conn)
	req, err := http.ReadRequest(br)

	if err != nil || req.Method != http.MethodConnect {
		return
	}

	p.mu.Lock()
	p.target = req.Host
	p.authorization = req.Header.Get("Proxy-Authorization")
	p.mu.Unlock()

	notify(p.connected)

	switch {
	case p.stall:
		// Read until the other side gives up and closes the connection.
		_, _ = io.Copy(io.Discard, br)
		notify(p.closed)

	case p.status != http.StatusOK:
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\n\r\n", p.status, http.StatusText(p.status))

	case p.early != "":
		// One write, so that the bytes most likely arrive with the 200 and
		// are read together with it.
		_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n"+p.early)

		if _, err := http.ReadRequest(br); err != nil {
			return
		}

		_, _ = io.Copy(conn, br)

	default:
		upstream, err := net.Dial("tcp", req.Host)

		if err != nil {
			_, _ = io.WriteString(conn, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
			return
		}

		defer func() { _ = upstream.Close() }()

		_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection established\r\n\r\n")

		go func() {
			_, _ = io.Copy(upstream, br)
			closeWrite(upstream)
		}()

		_, _ = io.Copy(conn, upstream)
	}
}

func TestUpgradeThroughConnectProxy(t *testing.T) {
	upstream := newUpgradeUpstream(t)
	proxy := newConnectProxy(t, nil)

	kubeconfig := writeClusterKubeconfig(t, &clientcmdapi.Cluster{
		Server:                   upstream.URL,
		CertificateAuthorityData: caData(upstream.Server),
		ProxyURL:                 proxy.url("user:secret@"),
	})

	p := startProxy(t, map[string]string{"KUBECONFIG": kubeconfig})

	conn, br, resp := upgrade(t, p.addr, "/api/v1/namespaces/default/pods/p/exec")

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}

	expectEcho(t, conn, br)

	target, authorization := proxy.lastConnect()

	if want := strings.TrimPrefix(upstream.URL, "https://"); target != want {
		t.Errorf("CONNECT %q, want %q", target, want)
	}

	if want := "Basic " + base64.StdEncoding.EncodeToString([]byte("user:secret")); authorization != want {
		t.Errorf("Proxy-Authorization = %q, want %q", authorization, want)
	}

	// TLS goes through the tunnel, so the API server still gets the token.
	if _, _, authorization, _ := upstream.lastRequest(); authorization != "Bearer "+testBearerToken {
		t.Errorf("the API server got Authorization %q, want the token of the kubeconfig", authorization)
	}
}

func TestUpgradeConnectProxyRefuses(t *testing.T) {
	upstream := newUpgradeUpstream(t)
	proxy := newConnectProxy(t, func(p *connectProxy) { p.status = http.StatusProxyAuthRequired })

	kubeconfig := writeClusterKubeconfig(t, &clientcmdapi.Cluster{
		Server:                   upstream.URL,
		CertificateAuthorityData: caData(upstream.Server),
		ProxyURL:                 proxy.url(""),
	})

	p := startProxy(t, map[string]string{"KUBECONFIG": kubeconfig})

	_, _, resp := upgrade(t, p.addr, "/api/v1/namespaces/default/pods/p/exec")

	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)

	if !strings.Contains(string(body), "407") {
		t.Errorf("body = %q, want the status of the proxy", body)
	}
}

func TestUpgradeConnectProxyEarlyBytes(t *testing.T) {
	// The proxy answers the upgrade itself in the bytes it sends with the
	// 200, which a reader of the CONNECT response buffers. The upstream is
	// plain HTTP, so no TLS handshake has to come first, and its name is
	// resolved by the proxy only.
	proxy := newConnectProxy(t, func(p *connectProxy) {
		p.early = "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n"
	})

	kubeconfig := writeClusterKubeconfig(t, &clientcmdapi.Cluster{
		Server:   "http://kubernetes.test:6443",
		ProxyURL: proxy.url(""),
	})

	p := startProxy(t, map[string]string{"KUBECONFIG": kubeconfig})

	conn, br, resp := upgrade(t, p.addr, "/api/v1/namespaces/default/pods/p/exec")

	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}

	expectEcho(t, conn, br)

	if target, _ := proxy.lastConnect(); target != "kubernetes.test:6443" {
		t.Errorf("CONNECT %q, want kubernetes.test:6443", target)
	}
}

func TestUpgradeConnectProxyStalls(t *testing.T) {
	upstream := newUpgradeUpstream(t)
	proxy := newConnectProxy(t, func(p *connectProxy) { p.stall = true })

	kubeconfig := writeClusterKubeconfig(t, &clientcmdapi.Cluster{
		Server:                   upstream.URL,
		CertificateAuthorityData: caData(upstream.Server),
		ProxyURL:                 proxy.url(""),
	})

	p := startProxy(t, map[string]string{"KUBECONFIG": kubeconfig})

	conn, err := net.DialTimeout("tcp", p.addr, 5*time.Second)

	if err != nil {
		t.Fatalf("failed to connect to the proxy: %v", err)
	}

	_, err = fmt.Fprintf(conn, "GET /api/v1/namespaces/default/pods/p/exec HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n", p.addr)

	if err != nil {
		t.Fatalf("failed to send the upgrade request: %v", err)
	}

	select {
	case <-proxy.connected:
	case <-time.After(5 * time.Second):
		t.Fatal("the proxy got no CONNECT")
	}

	// The client gives up, which cancels the request. The CONNECT must end
	// then, not after the dial timeout of 30 seconds.
	_ = conn.Close()

	select {
	case <-proxy.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the CONNECT did not end when the request was cancelled")
	}
}
