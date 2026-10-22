package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	_ "k8s.io/client-go/plugin/pkg/client/auth"
	"k8s.io/kubectl/pkg/proxy"
)

// These get overridden at build time: -X main.version=$VERSION
var (
	version = "dev"
	commit  = ""
)

// Every setting but the kubeconfig, which keeps its standard name, is read
// from a variable with this prefix.
const envPrefix = "FREELENS_PROXY"

type VersionData struct {
	Version string `json:"gitVersion"`
	Commit  string `json:"gitCommit"`
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	v := viper.New()
	v.SetEnvPrefix(envPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))

	// Without a subcommand the root command prints the help.
	rootCmd := &cobra.Command{
		Use:     "freelens-k8s-proxy",
		Short:   "More secure alternative to kubectl proxy",
		Version: version,
		// Usage is for a mistake in the command line, not for a failure of
		// the command itself.
		SilenceUsage:      true,
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
	}

	rootCmd.AddCommand(newProxyCmd(v), newCredentialCmd(v), newVersionCmd())

	return rootCmd
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version as JSON",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			err := json.NewEncoder(cmd.OutOrStdout()).Encode(&VersionData{
				Version: version,
				Commit:  commit,
			})

			if err != nil {
				return fmt.Errorf("failed to marshal version data: %w", err)
			}

			return nil
		},
	}
}

func newProxyCmd(v *viper.Viper) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "proxy",
		Short: "Start the proxy to the Kubernetes API server",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runProxy(v)
		},
	}

	cmd.Flags().String("kubeconfig", "", "path to the kubeconfig file (env KUBECONFIG)")
	cmd.Flags().String("context", "", "name of the kubeconfig context (env FREELENS_PROXY_CONTEXT)")
	cmd.Flags().String("api-prefix", "", "URL prefix for API requests (env FREELENS_PROXY_API_PREFIX)")

	_ = v.BindPFlags(cmd.Flags())
	_ = v.BindEnv("context")
	_ = v.BindEnv("api-prefix")

	// The TLS certificate and key have no flags: a value on the command line
	// is visible in ps.
	_ = v.BindEnv("cert")
	_ = v.BindEnv("key")

	return cmd
}

func runProxy(v *viper.Viper) error {
	kubeconfig := v.GetString("kubeconfig")
	kubeconfigContext := v.GetString("context")
	apiPrefix := v.GetString("api-prefix")
	proxyCert := v.GetString("cert")
	proxyKey := v.GetString("key")

	if !strings.HasPrefix(apiPrefix, "/") {
		apiPrefix = "/" + apiPrefix
	}
	if !strings.HasSuffix(apiPrefix, "/") {
		apiPrefix += "/"
	}

	done := make(chan os.Signal, 2)
	signal.Notify(done, os.Interrupt, syscall.SIGTERM)

	fmt.Printf("~~ Freelens K8s Proxy, '%s' ~~\n", version)
	if kubeconfig != "" {
		fmt.Printf("kubeconfig: %s\n", kubeconfig)
	} else {
		fmt.Printf("kubeconfig: %s\n", os.Getenv("KUBECONFIG"))
	}
	fmt.Printf("api prefix: %s\n", apiPrefix)

	config := genericclioptions.NewConfigFlags(false)

	// KUBECONFIG is not read here: an explicit path is a single file, and
	// without one client-go loads KUBECONFIG, which may list several files,
	// or ~/.kube/config, as kubectl does.
	if kubeconfig != "" {
		config.KubeConfig = &kubeconfig
	}

	if kubeconfigContext != "" {
		config.Context = &kubeconfigContext
	}

	clientConfig, err := config.ToRESTConfig()

	if err != nil {
		return fmt.Errorf("failed to initialize kubeconfig: %w", err)
	}

	proxyHandler, err := proxy.NewProxyHandler(apiPrefix, nil, clientConfig, 0, true)

	if err != nil {
		return fmt.Errorf("failed to initialize proxy handler: %w", err)
	}

	upgradeHandler, err := newUpgradePipeHandler(apiPrefix, clientConfig)

	if err != nil {
		return fmt.Errorf("failed to initialize upgrade pipe handler: %w", err)
	}

	mux := http.NewServeMux()
	mux.Handle(apiPrefix, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isUpgradeRequest(r) {
			upgradeHandler.ServeHTTP(w, r)
			return
		}
		proxyHandler.ServeHTTP(w, r)
	}))

	l, err := getListener(proxyCert, proxyKey)

	if err != nil {
		return fmt.Errorf("failed to get listener: %w", err)
	}

	fmt.Printf("starting to serve on %s\n", l.Addr().String())

	httpServer := &http.Server{
		Handler: mux,
		// Go allows 250 concurrent streams by default, and every watch holds
		// one for hours. Match the limit the Freelens proxy gives the renderer.
		HTTP2: &http.HTTP2Config{MaxConcurrentStreams: 1000},
	}

	serveErr := make(chan error, 1)

	go func() {
		serveErr <- httpServer.Serve(l)
	}()

	select {
	case <-done:
	case err := <-serveErr:
		if err != http.ErrServerClosed {
			return err
		}
	}

	fmt.Println("shutting down ...")

	_ = l.Close()

	return nil
}

const proxyAddr = "127.0.0.1:0"

func getListener(proxyCert string, proxyKey string) (net.Listener, error) {
	if proxyCert == "" || proxyKey == "" {
		return net.Listen("tcp", proxyAddr)
	}

	cert, err := tls.X509KeyPair([]byte(proxyCert), []byte(proxyKey))
	if err != nil {
		return nil, err
	}

	return tls.Listen("tcp", proxyAddr, &tls.Config{
		Certificates: []tls.Certificate{cert},
		// Offer HTTP/2 through ALPN. A client that sends no ALPN, as the
		// current Freelens does, and upgrades (exec, attach, port-forward),
		// which need a hijackable HTTP/1.1 connection, stay on HTTP/1.1.
		NextProtos: []string{"h2", "http/1.1"},
		MinVersion: tls.VersionTLS12, // Set the minimum version of TLS to 1.2
		MaxVersion: tls.VersionTLS13, // Set the maximum version of TLS to 1.3
		CipherSuites: []uint16{
			tls.TLS_AES_256_GCM_SHA384,
			tls.TLS_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
		},
	})
}
