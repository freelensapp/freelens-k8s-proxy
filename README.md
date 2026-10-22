# Freelens K8s Proxy

<!-- markdownlint-disable MD013 -->

[![GitHub](https://img.shields.io/github/v/release/freelensapp/freelens-k8s-proxy?display_name=tag&sort=semver)](https://github.com/freelensapp/freelens-k8s-proxy)
[![Test](https://github.com/freelensapp/freelens-k8s-proxy/actions/workflows/test.yaml/badge.svg)](https://github.com/freelensapp/freelens-k8s-proxy/actions/workflows/test.yaml)

<!-- markdownlint-enable MD013 -->

More secure alternative to `kubectl proxy`, and a helper for the
kubeconfigs that Freelens generates.

## Usage

| Command                         | Purpose                          |
| ------------------------------- | -------------------------------- |
| `freelens-k8s-proxy`            | Prints the help                  |
| `freelens-k8s-proxy proxy`      | Starts the proxy                 |
| `freelens-k8s-proxy credential` | Prints an `ExecCredential`       |
| `freelens-k8s-proxy version`    | Prints the version as JSON       |
| `freelens-k8s-proxy --version`  | Prints the version as plain text |

`version` prints the same JSON as v1, with `gitVersion` and `gitCommit`.

### Variables and flags

`FREELENS_PROXY_TOKEN` is for `credential`, the others are for `proxy`. A
flag takes precedence over its variable.

| Variable                    | Flag           | Purpose                     |
| --------------------------- | -------------- | --------------------------- |
| `KUBECONFIG`                | `--kubeconfig` | Path to the kubeconfig file |
| `FREELENS_PROXY_CONTEXT`    | `--context`    | Kubeconfig context name     |
| `FREELENS_PROXY_API_PREFIX` | `--api-prefix` | URL prefix for API requests |
| `FREELENS_PROXY_CERT`       | none           | TLS certificate (PEM)       |
| `FREELENS_PROXY_KEY`        | none           | TLS private key (PEM)       |
| `FREELENS_PROXY_TOKEN`      | none           | Token for `credential`      |

`KUBECONFIG` works as for kubectl: it may list several files, and without it
the proxy reads `~/.kube/config`. `--kubeconfig` names a single file.

Secrets have no flags: a value on the command line is visible in `ps` and,
for `credential`, would end up in the `args` of the kubeconfig.

The proxy listens on a random port of `127.0.0.1` and prints
`starting to serve on <address>`. With `FREELENS_PROXY_CERT` and
`FREELENS_PROXY_KEY` it listens with TLS, otherwise with plain HTTP.

### Credential

`credential` is a Kubernetes
[exec credential plugin](https://kubernetes.io/docs/reference/access-authn-authz/authentication/#client-go-credential-plugins).
It prints the token from `FREELENS_PROXY_TOKEN` on stdout:

```json
{
  "apiVersion": "client.authentication.k8s.io/v1",
  "kind": "ExecCredential",
  "status": { "token": "<value of FREELENS_PROXY_TOKEN>" }
}
```

A kubeconfig names the binary under `users[].exec`, with its absolute path:

```yaml
users:
  - name: freelens
    user:
      exec:
        apiVersion: client.authentication.k8s.io/v1
        command: /path/to/freelens-k8s-proxy
        args:
          - credential
        interactiveMode: Never
```

kubectl, helm and other clients that use client-go then send the token as
`Authorization: Bearer <token>`.

- The `apiVersion` is the one that `KUBERNETES_EXEC_INFO` asks for,
  `client.authentication.k8s.io/v1` or `client.authentication.k8s.io/v1beta1`,
  and `client.authentication.k8s.io/v1` without the variable. Any other
  version is an error.
- There is no `expirationTimestamp`, so the client keeps the token for the
  life of its process.
- When `FREELENS_PROXY_TOKEN` is missing or empty, it prints an error on
  stderr, nothing on stdout, and exits with a non-zero code.
- It reads no kubeconfig, opens no listener and makes no network call.

The token is not stored in the kubeconfig on disk, but it still reaches every
process started with `FREELENS_PROXY_TOKEN` in its environment, as every
process of a Freelens terminal is.

## How to build

On Mac and Linux install tools using [mise](https://mise.jdx.dev/):

```sh
mise install
make download
make build
```

On Windows:

```powershell
winget install GoLang.Go
winget install goreleaser.goreleaser
./Makefile.ps1 download
./Makefile.ps1 build
```

## How to verify a release

Every released binary is published with an SPDX SBOM
(`<binary>.sbom.json`) and a build provenance attestation. Both the binaries
and the SBOMs can be verified with the GitHub CLI:

```sh
gh attestation verify freelens-k8s-proxy-linux-amd64 --repo freelensapp/freelens-k8s-proxy
```

Note that `gh attestation verify` prints its result only on a terminal: in CI
or when its output is piped it reports through the exit status alone. Use
`--format json` when a script needs the details.

## License

This repository is a fork of [lens-k8s-proxy](https://github.com/lensapp/lens-k8s-proxy/tree/main).

Copyright (c) 2024-2025 Freelens Authors.

Copyright (c) 2022 Mirantis, Inc.

[MIT License](https://opensource.org/licenses/MIT)
