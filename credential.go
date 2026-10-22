package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// client-go describes the request to an exec credential plugin in this
// variable, and requires the response to have the same apiVersion.
const execInfoEnv = "KUBERNETES_EXEC_INFO"

const defaultExecCredentialAPIVersion = "client.authentication.k8s.io/v1"

var supportedExecCredentialAPIVersions = map[string]bool{
	"client.authentication.k8s.io/v1":      true,
	"client.authentication.k8s.io/v1beta1": true,
}

// ExecCredential is the response of an exec credential plugin. It has no
// expirationTimestamp, so client-go keeps the token for the life of the
// process.
type ExecCredential struct {
	APIVersion string               `json:"apiVersion"`
	Kind       string               `json:"kind"`
	Status     ExecCredentialStatus `json:"status"`
}

type ExecCredentialStatus struct {
	Token string `json:"token"`
}

func newCredentialCmd(v *viper.Viper) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "credential",
		Short: "Print an ExecCredential with the token from FREELENS_PROXY_TOKEN",
		Long: "Print an ExecCredential with the token from FREELENS_PROXY_TOKEN, for\n" +
			"users[].exec of a kubeconfig. It reads no kubeconfig and makes no\n" +
			"network call.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCredential(cmd, v)
		},
	}

	// The token has no flag: a value on the command line is visible in ps
	// and would end up in the args of the kubeconfig.
	_ = v.BindEnv("token")

	return cmd
}

func runCredential(cmd *cobra.Command, v *viper.Viper) error {
	// The token never goes into an error message.
	token := v.GetString("token")

	if token == "" {
		return fmt.Errorf("%s_TOKEN is not set or is empty", envPrefix)
	}

	apiVersion, err := execCredentialAPIVersion(os.Getenv(execInfoEnv))

	if err != nil {
		return err
	}

	err = json.NewEncoder(cmd.OutOrStdout()).Encode(&ExecCredential{
		APIVersion: apiVersion,
		Kind:       "ExecCredential",
		Status:     ExecCredentialStatus{Token: token},
	})

	if err != nil {
		return fmt.Errorf("failed to marshal exec credential: %w", err)
	}

	return nil
}

func execCredentialAPIVersion(execInfo string) (string, error) {
	if execInfo == "" {
		return defaultExecCredentialAPIVersion, nil
	}

	var info struct {
		APIVersion string `json:"apiVersion"`
	}

	if err := json.Unmarshal([]byte(execInfo), &info); err != nil {
		return "", fmt.Errorf("failed to parse %s: %w", execInfoEnv, err)
	}

	if !supportedExecCredentialAPIVersions[info.APIVersion] {
		return "", fmt.Errorf("unsupported apiVersion %q in %s", info.APIVersion, execInfoEnv)
	}

	return info.APIVersion, nil
}
