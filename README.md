# Freelens K8s Proxy

<!-- markdownlint-disable MD013 -->

[![GitHub](https://img.shields.io/github/v/release/freelensapp/freelens-k8s-proxy?display_name=tag&sort=semver)](https://github.com/freelensapp/freelens-k8s-proxy)
[![Test](https://github.com/freelensapp/freelens-k8s-proxy/actions/workflows/test.yaml/badge.svg)](https://github.com/freelensapp/freelens-k8s-proxy/actions/workflows/test.yaml)

<!-- markdownlint-enable MD013 -->

More secure alternative to `kubectl proxy`.

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
