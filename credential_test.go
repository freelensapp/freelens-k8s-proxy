package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

const testToken = "test-token-value"

// setEnv sets the variable for the test, or unsets it when value is nil.
func setEnv(t *testing.T, key string, value *string) {
	t.Helper()

	if value == nil {
		// Setenv restores the previous value after the test.
		t.Setenv(key, "")
		_ = os.Unsetenv(key)
		return
	}

	t.Setenv(key, *value)
}

func runCredentialCmd(t *testing.T, token *string, execInfo *string) (string, string, error) {
	t.Helper()

	setEnv(t, "FREELENS_PROXY_TOKEN", token)
	setEnv(t, "KUBERNETES_EXEC_INFO", execInfo)

	var stdout, stderr bytes.Buffer

	cmd := newRootCmd()
	cmd.SetArgs([]string{"credential"})
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)

	err := cmd.Execute()

	return stdout.String(), stderr.String(), err
}

func ptr(s string) *string {
	return &s
}

func TestCredential(t *testing.T) {
	tests := []struct {
		name       string
		execInfo   *string
		apiVersion string
	}{
		{
			name:       "without KUBERNETES_EXEC_INFO",
			execInfo:   nil,
			apiVersion: "client.authentication.k8s.io/v1",
		},
		{
			name:       "with v1",
			execInfo:   ptr(`{"kind":"ExecCredential","apiVersion":"client.authentication.k8s.io/v1","spec":{"interactive":false}}`),
			apiVersion: "client.authentication.k8s.io/v1",
		},
		{
			name:       "with v1beta1",
			execInfo:   ptr(`{"kind":"ExecCredential","apiVersion":"client.authentication.k8s.io/v1beta1","spec":{"interactive":false}}`),
			apiVersion: "client.authentication.k8s.io/v1beta1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, stderr, err := runCredentialCmd(t, ptr(testToken), tt.execInfo)

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if stderr != "" {
				t.Errorf("unexpected stderr: %q", stderr)
			}

			// stdout holds the ExecCredential and nothing else.
			var got map[string]any

			decoder := json.NewDecoder(strings.NewReader(stdout))

			if err := decoder.Decode(&got); err != nil {
				t.Fatalf("stdout is not JSON: %v: %q", err, stdout)
			}

			if decoder.More() {
				t.Errorf("stdout has more than one JSON value: %q", stdout)
			}

			want := map[string]any{
				"apiVersion": tt.apiVersion,
				"kind":       "ExecCredential",
				"status":     map[string]any{"token": testToken},
			}

			gotJSON, _ := json.Marshal(got)
			wantJSON, _ := json.Marshal(want)

			if !bytes.Equal(gotJSON, wantJSON) {
				t.Errorf("stdout = %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}

func TestCredentialError(t *testing.T) {
	tests := []struct {
		name     string
		token    *string
		execInfo *string
		message  string
	}{
		{
			name:    "missing token",
			token:   nil,
			message: "FREELENS_PROXY_TOKEN",
		},
		{
			name:    "empty token",
			token:   ptr(""),
			message: "FREELENS_PROXY_TOKEN",
		},
		{
			name:     "unsupported apiVersion",
			token:    ptr(testToken),
			execInfo: ptr(`{"kind":"ExecCredential","apiVersion":"client.authentication.k8s.io/v1alpha1"}`),
			message:  "client.authentication.k8s.io/v1alpha1",
		},
		{
			name:     "invalid KUBERNETES_EXEC_INFO",
			token:    ptr(testToken),
			execInfo: ptr(`not json`),
			message:  "KUBERNETES_EXEC_INFO",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stdout, stderr, err := runCredentialCmd(t, tt.token, tt.execInfo)

			if err == nil {
				t.Fatal("expected an error")
			}

			if stdout != "" {
				t.Errorf("unexpected stdout: %q", stdout)
			}

			if !strings.Contains(stderr, tt.message) {
				t.Errorf("stderr = %q, want it to contain %q", stderr, tt.message)
			}

			if strings.Contains(stderr, testToken) {
				t.Errorf("stderr contains the token: %q", stderr)
			}
		})
	}
}
