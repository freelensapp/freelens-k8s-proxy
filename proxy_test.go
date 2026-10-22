package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
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

const testBearerToken = "test-bearer-token"

// fakeAPIServer stands in for the Kubernetes API server. It serves TLS, as
// client-go sends the credentials of a kubeconfig only to an https server.
// It answers every request with its name and records the path and the Authorization header
// of the last one.
type fakeAPIServer struct {
	*httptest.Server

	mu            sync.Mutex
	path          string
	authorization string
}

func newFakeAPIServer(t *testing.T, name string) *fakeAPIServer {
	t.Helper()

	s := &fakeAPIServer{}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.path = r.URL.Path
		s.authorization = r.Header.Get("Authorization")
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"gitVersion": name})
	}))
	t.Cleanup(s.Close)

	return s
}

func (s *fakeAPIServer) lastRequest() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.path, s.authorization
}

// writeKubeconfig writes a kubeconfig with a context for each server, named
// after it, and the given current context.
func writeKubeconfig(t *testing.T, servers map[string]*fakeAPIServer, current string) string {
	t.Helper()

	config := clientcmdapi.NewConfig()
	config.AuthInfos["user"] = &clientcmdapi.AuthInfo{Token: testBearerToken}

	for name, server := range servers {
		config.Clusters[name] = &clientcmdapi.Cluster{
			Server:                   server.URL,
			CertificateAuthorityData: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}),
		}
		config.Contexts[name] = &clientcmdapi.Context{Cluster: name, AuthInfo: "user"}
	}

	config.CurrentContext = current

	path := filepath.Join(t.TempDir(), "kubeconfig")

	if err := clientcmd.WriteToFile(*config, path); err != nil {
		t.Fatalf("failed to write kubeconfig: %v", err)
	}

	return path
}

// proxyOutput collects the stdout of the proxy, which the test reads while
// the proxy still writes, and reports the address it serves on.
type proxyOutput struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	addr chan string
}

func (o *proxyOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	// The proxy prints each line with a write of its own.
	if addr, ok := strings.CutPrefix(strings.TrimSpace(string(p)), "starting to serve on "); ok {
		o.addr <- addr
	}

	return o.buf.Write(p)
}

func (o *proxyOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.buf.String()
}

type runningProxy struct {
	addr   string
	out    *proxyOutput
	cancel context.CancelFunc
	result chan error

	stopOnce sync.Once
	stopErr  error
}

// stop stops the proxy and returns the error of the command. A test may
// call it before the cleanup does.
func (p *runningProxy) stop(t *testing.T) error {
	t.Helper()

	p.stopOnce.Do(func() {
		p.cancel()

		select {
		case p.stopErr = <-p.result:
		case <-time.After(10 * time.Second):
			t.Error("the proxy did not stop")
		}
	})

	return p.stopErr
}

// proxyEnv clears every variable the proxy reads, so that the environment
// of the test run cannot leak into a test, and then sets the given ones.
func proxyEnv(t *testing.T, env map[string]string) {
	t.Helper()

	for _, key := range []string{
		"KUBECONFIG",
		"FREELENS_PROXY_CONTEXT",
		"FREELENS_PROXY_API_PREFIX",
		"FREELENS_PROXY_CERT",
		"FREELENS_PROXY_KEY",
	} {
		t.Setenv(key, env[key])
	}
}

// executeProxy runs the proxy command until it serves or fails. It returns
// the running proxy, or the output and the error of a failed start.
func executeProxy(t *testing.T, env map[string]string, args ...string) (*runningProxy, string, error) {
	t.Helper()

	proxyEnv(t, env)

	out := &proxyOutput{addr: make(chan string, 1)}

	cmd := newRootCmd()
	cmd.SetArgs(append([]string{"proxy"}, args...))
	cmd.SetOut(out)
	cmd.SetErr(io.Discard)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)

	go func() {
		result <- cmd.ExecuteContext(ctx)
	}()

	select {
	case addr := <-out.addr:
		p := &runningProxy{addr: addr, out: out, cancel: cancel, result: result}
		t.Cleanup(func() { _ = p.stop(t) })
		return p, "", nil
	case err := <-result:
		cancel()
		if err == nil {
			t.Fatal("the proxy exited without an error before it served")
		}
		return nil, out.String(), err
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("the proxy did not start")
		return nil, "", nil
	}
}

func startProxy(t *testing.T, env map[string]string, args ...string) *runningProxy {
	t.Helper()

	p, _, err := executeProxy(t, env, args...)

	if err != nil {
		t.Fatalf("the proxy failed to start: %v", err)
	}

	return p
}

// getVersion sends GET to the URL and returns the status and, for 200, the
// name of the fake API server that answered.
func getVersion(t *testing.T, client *http.Client, url string) (int, string) {
	t.Helper()

	resp, err := client.Get(url)

	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}

	defer func() { _ = resp.Body.Close() }()

	var body struct {
		GitVersion string `json:"gitVersion"`
	}

	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("GET %s: the body is not JSON: %v", url, err)
		}
	}

	return resp.StatusCode, body.GitVersion
}

var testClient = &http.Client{Timeout: 10 * time.Second}

func TestProxyForwardsRequest(t *testing.T) {
	server := newFakeAPIServer(t, "a")
	kubeconfig := writeKubeconfig(t, map[string]*fakeAPIServer{"a": server}, "a")

	p := startProxy(t, map[string]string{"KUBECONFIG": kubeconfig})

	status, name := getVersion(t, testClient, "http://"+p.addr+"/version")

	if status != http.StatusOK || name != "a" {
		t.Fatalf("got %d from %q, want 200 from \"a\"", status, name)
	}

	path, authorization := server.lastRequest()

	if path != "/version" {
		t.Errorf("the API server got the path %q, want /version", path)
	}

	// The proxy authenticates with the credentials of the kubeconfig, which
	// its client never sees.
	if authorization != "Bearer "+testBearerToken {
		t.Errorf("the API server got Authorization %q, want the token of the kubeconfig", authorization)
	}
}

func TestProxyAPIPrefix(t *testing.T) {
	tests := []struct {
		apiPrefix string
		want      string
	}{
		{apiPrefix: "", want: "/"},
		{apiPrefix: "/", want: "/"},
		{apiPrefix: "foo", want: "/foo/"},
		{apiPrefix: "/foo", want: "/foo/"},
		{apiPrefix: "foo/", want: "/foo/"},
		{apiPrefix: "/foo/", want: "/foo/"},
	}

	for _, tt := range tests {
		t.Run(tt.apiPrefix, func(t *testing.T) {
			server := newFakeAPIServer(t, "a")
			kubeconfig := writeKubeconfig(t, map[string]*fakeAPIServer{"a": server}, "a")

			p := startProxy(t, map[string]string{
				"KUBECONFIG":                kubeconfig,
				"FREELENS_PROXY_API_PREFIX": tt.apiPrefix,
			})

			if !strings.Contains(p.out.String(), "api prefix: "+tt.want+"\n") {
				t.Errorf("output = %q, want the prefix %s", p.out.String(), tt.want)
			}

			status, _ := getVersion(t, testClient, "http://"+p.addr+tt.want+"version")

			if status != http.StatusOK {
				t.Fatalf("GET %sversion: %d, want 200", tt.want, status)
			}

			// The prefix is stripped before the request goes upstream.
			if path, _ := server.lastRequest(); path != "/version" {
				t.Errorf("the API server got the path %q, want /version", path)
			}

			if tt.want != "/" {
				if status, _ := getVersion(t, testClient, "http://"+p.addr+"/version"); status != http.StatusNotFound {
					t.Errorf("GET /version outside the prefix: %d, want 404", status)
				}
			}
		})
	}
}

func TestProxyFlagOverridesVariable(t *testing.T) {
	servers := map[string]*fakeAPIServer{
		"a": newFakeAPIServer(t, "a"),
		"b": newFakeAPIServer(t, "b"),
	}
	kubeconfig := writeKubeconfig(t, servers, "a")
	otherKubeconfig := writeKubeconfig(t, map[string]*fakeAPIServer{"b": servers["b"]}, "b")

	tests := []struct {
		name   string
		env    map[string]string
		args   []string
		prefix string
		want   string
	}{
		{
			name: "current context",
			env:  map[string]string{"KUBECONFIG": kubeconfig},
			want: "a",
		},
		{
			name: "context from the variable",
			env:  map[string]string{"KUBECONFIG": kubeconfig, "FREELENS_PROXY_CONTEXT": "b"},
			want: "b",
		},
		{
			name: "context from the flag",
			env:  map[string]string{"KUBECONFIG": kubeconfig, "FREELENS_PROXY_CONTEXT": "a"},
			args: []string{"--context", "b"},
			want: "b",
		},
		{
			name: "kubeconfig from the flag",
			env:  map[string]string{"KUBECONFIG": kubeconfig},
			args: []string{"--kubeconfig", otherKubeconfig},
			want: "b",
		},
		{
			name:   "API prefix from the flag",
			env:    map[string]string{"KUBECONFIG": kubeconfig, "FREELENS_PROXY_API_PREFIX": "foo"},
			args:   []string{"--api-prefix", "bar"},
			prefix: "/bar/",
			want:   "a",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := startProxy(t, tt.env, tt.args...)

			prefix := tt.prefix

			if prefix == "" {
				prefix = "/"
			}

			status, name := getVersion(t, testClient, "http://"+p.addr+prefix+"version")

			if status != http.StatusOK || name != tt.want {
				t.Errorf("got %d from %q, want 200 from %q", status, name, tt.want)
			}
		})
	}
}

func TestProxyKubeconfigList(t *testing.T) {
	first := writeKubeconfig(t, map[string]*fakeAPIServer{"a": newFakeAPIServer(t, "a")}, "a")
	second := writeKubeconfig(t, map[string]*fakeAPIServer{"b": newFakeAPIServer(t, "b")}, "b")

	// As for kubectl, KUBECONFIG may list several files, which are merged.
	p := startProxy(t, map[string]string{
		"KUBECONFIG":             first + string(filepath.ListSeparator) + second,
		"FREELENS_PROXY_CONTEXT": "b",
	})

	status, name := getVersion(t, testClient, "http://"+p.addr+"/version")

	if status != http.StatusOK || name != "b" {
		t.Errorf("got %d from %q, want 200 from \"b\"", status, name)
	}
}

// selfSignedCert returns a PEM certificate and key for 127.0.0.1, and a pool
// that trusts the certificate.
func selfSignedCert(t *testing.T) (string, string, *x509.CertPool) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)

	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "freelens-k8s-proxy test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)

	if err != nil {
		t.Fatalf("failed to create certificate: %v", err)
	}

	keyDER, err := x509.MarshalECPrivateKey(key)

	if err != nil {
		t.Fatalf("failed to marshal key: %v", err)
	}

	cert, err := x509.ParseCertificate(der)

	if err != nil {
		t.Fatalf("failed to parse certificate: %v", err)
	}

	pool := x509.NewCertPool()
	pool.AddCert(cert)

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})),
		pool
}

func TestProxyTLS(t *testing.T) {
	kubeconfig := writeKubeconfig(t, map[string]*fakeAPIServer{"a": newFakeAPIServer(t, "a")}, "a")
	cert, key, pool := selfSignedCert(t)

	p := startProxy(t, map[string]string{
		"KUBECONFIG":          kubeconfig,
		"FREELENS_PROXY_CERT": cert,
		"FREELENS_PROXY_KEY":  key,
	})

	t.Run("HTTPS", func(t *testing.T) {
		client := &http.Client{
			Timeout:   10 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}},
		}

		status, name := getVersion(t, client, "https://"+p.addr+"/version")

		if status != http.StatusOK || name != "a" {
			t.Errorf("got %d from %q, want 200 from \"a\"", status, name)
		}
	})

	t.Run("plain HTTP is refused", func(t *testing.T) {
		resp, err := testClient.Get("http://" + p.addr + "/version")

		if err == nil {
			_ = resp.Body.Close()

			if resp.StatusCode == http.StatusOK {
				t.Error("the TLS listener served plain HTTP")
			}
		}
	})

	tests := []struct {
		name       string
		nextProtos []string
		want       string
	}{
		{name: "ALPN offers HTTP/2", nextProtos: []string{"h2", "http/1.1"}, want: "h2"},
		{name: "ALPN keeps HTTP/1.1", nextProtos: []string{"http/1.1"}, want: "http/1.1"},
		{name: "without ALPN", nextProtos: nil, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conn, err := tls.Dial("tcp", p.addr, &tls.Config{RootCAs: pool, NextProtos: tt.nextProtos})

			if err != nil {
				t.Fatalf("TLS handshake failed: %v", err)
			}

			defer func() { _ = conn.Close() }()

			if got := conn.ConnectionState().NegotiatedProtocol; got != tt.want {
				t.Errorf("negotiated protocol = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("TLS 1.1 is refused", func(t *testing.T) {
		conn, err := tls.Dial("tcp", p.addr, &tls.Config{
			RootCAs:    pool,
			MinVersion: tls.VersionTLS10,
			MaxVersion: tls.VersionTLS11,
		})

		if err == nil {
			_ = conn.Close()
			t.Error("the TLS handshake with TLS 1.1 succeeded")
		}
	})
}

func TestProxyStartError(t *testing.T) {
	kubeconfig := writeKubeconfig(t, map[string]*fakeAPIServer{"a": newFakeAPIServer(t, "a")}, "a")

	tests := []struct {
		name    string
		env     map[string]string
		args    []string
		message string
	}{
		{
			name:    "missing kubeconfig",
			args:    []string{"--kubeconfig", filepath.Join(t.TempDir(), "missing")},
			message: "failed to initialize kubeconfig",
		},
		{
			name:    "unknown context",
			env:     map[string]string{"KUBECONFIG": kubeconfig, "FREELENS_PROXY_CONTEXT": "missing"},
			message: "failed to initialize kubeconfig",
		},
		{
			name: "invalid certificate",
			env: map[string]string{
				"KUBECONFIG":          kubeconfig,
				"FREELENS_PROXY_CERT": "not a certificate",
				"FREELENS_PROXY_KEY":  "not a key",
			},
			message: "failed to get listener",
		},
		{
			name:    "arguments",
			env:     map[string]string{"KUBECONFIG": kubeconfig},
			args:    []string{"extra"},
			message: "unknown command",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, out, err := executeProxy(t, tt.env, tt.args...)

			if err == nil || !strings.Contains(err.Error(), tt.message) {
				t.Errorf("error = %v, want it to contain %q", err, tt.message)
			}

			// Freelens waits for this line, so a failed start must not print it.
			if strings.Contains(out, "starting to serve on") {
				t.Errorf("output = %q, want no listener", out)
			}
		})
	}
}

func TestProxyStop(t *testing.T) {
	kubeconfig := writeKubeconfig(t, map[string]*fakeAPIServer{"a": newFakeAPIServer(t, "a")}, "a")

	p := startProxy(t, map[string]string{"KUBECONFIG": kubeconfig})

	if err := p.stop(t); err != nil {
		t.Fatalf("the proxy stopped with an error: %v", err)
	}

	if !strings.HasSuffix(p.out.String(), "shutting down ...\n") {
		t.Errorf("output = %q, want it to end with the shutdown message", p.out.String())
	}

	if conn, err := net.DialTimeout("tcp", p.addr, time.Second); err == nil {
		_ = conn.Close()
		t.Error("the proxy still listens after it stopped")
	}
}
