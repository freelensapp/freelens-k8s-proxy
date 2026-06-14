# Agent Guide: Freelens K8s Proxy Development

## Overview

This guide helps AI agents understand the Freelens K8s Proxy codebase, common
development tasks, troubleshooting patterns, and key architectural decisions.
Use this as a reference when working on the project.

**freelens-k8s-proxy** is a more secure alternative to `kubectl proxy`. It is a
single Go binary that starts an HTTP(S) proxy to the Kubernetes API server
using a kubeconfig, with optional TLS support.

- **`main.go`** — Single entry point; all application logic
- **`go.mod`** / **`go.sum`** — Go module dependencies
- **`Makefile`** — Build and development targets
- **`.goreleaser.yaml`** — Release build configuration
- **`.trunk/`** — Trunk.io linter/formatter configuration

## Security

Never read, display, reference, or include the contents of the following files
in any response or context, even if they are open in the editor:

- `.env`
- `.env.*`
- `*.jks`
- `*.keystore`
- `*.p12`
- `*.pfx`
- `*.pem`
- `*.key`

## Build System

### Prerequisites

On macOS and Linux, use [mise](https://mise.jdx.dev/) to manage tools:

```sh
mise install
```

This installs the Go version specified in `.go-version` and tools specified in `mise.toml`.

### Commands

```bash
make download          # Download Go modules
make build             # Build binary for current platform (via goreleaser)
make install           # Build and install binary to $PATH
make tidy              # Tidy Go modules
make upgrade           # Upgrade Go modules
make clean             # Remove built binary and dist/
```

### Build Details

The build uses [goreleaser](https://goreleaser.com/) (configured in
`.goreleaser.yaml`) which produces a statically-linked binary (`CGO_ENABLED=0`)
targeting `amd64` and `arm64` on `darwin`, `linux`, and `windows`.

Version information is injected at build time via linker flags (`-ldflags`):

```go
var (
    version = "dev"
    commit  = ""
)
```

Set these with:

```bash
-X main.version=$VERSION -X main.commit=$COMMIT
```

### Clean Build

When facing caching issues:

```bash
make clean
make download
make build
```

## Project Structure

```text
freelens-k8s-proxy/
├── main.go              # Application entry point
├── go.mod               # Go module definition
├── go.sum               # Dependency checksums
├── Makefile             # Build targets
├── Makefile.ps1         # Windows PowerShell build targets
├── .goreleaser.yaml     # Release build config
├── .trunk/              # Trunk linter configuration
├── .go-version          # Go version (used by mise)
├── mise.toml            # Additional mise tools (goreleaser)
└── .github/             # CI workflows
```

## Common Development Tasks

### Adding a CLI Subcommand

The binary supports a `version` subcommand. To add a new subcommand:

1. Add the command name check in `main()` alongside the existing `version` check
2. Implement the logic in a new function or inline
3. Follow the same pattern of writing JSON to stdout for machine-readable
   output

### Changing Proxy Behavior

The proxy is created using `k8s.io/kubectl/pkg/proxy`. Key configuration comes
from environment variables:

| Variable             | Purpose                      |
|----------------------|------------------------------|
| `KUBECONFIG`         | Path to kubeconfig file      |
| `KUBECONFIG_CONTEXT` | Kubeconfig context name      |
| `API_PREFIX`         | URL prefix for API requests  |
| `PROXY_CERT`         | TLS certificate (PEM)        |
| `PROXY_KEY`          | TLS private key (PEM)        |

### Updating Dependencies

```bash
make upgrade            # Upgrade all Go modules
make tidy               # Clean up go.mod/go.sum
make build              # Verify build still works
```

### Running Locally

```bash
make build
KUBECONFIG=~/.kube/config ./freelens-k8s-proxy
```

## Troubleshooting Patterns

### Build Failures

1. Check Go version matches `.go-version`: `go version`
2. Verify modules are downloaded: `go mod download`
3. Check for syntax errors: `go vet ./...`
4. Verify goreleaser config: `goreleaser check`
5. Clean and rebuild: `make clean && make build`

### Runtime Errors

1. Verify `KUBECONFIG` points to a valid file
2. Check `kubectl cluster-info` works with the same kubeconfig
3. Ensure TLS certificates are valid if using `PROXY_CERT`/`PROXY_KEY`
4. Check the proxy is listening: look for `starting to serve on` in output

### Linting Errors

Run Trunk to check all linters:

```bash
trunk check
```

For auto-fixing:

```bash
trunk fmt
```

The project uses:
- `gofmt` — Go formatting
- `golangci-lint2` — Go static analysis
- `actionlint` — GitHub Actions validation
- `markdownlint` — Markdown linting
- `yamllint` / `yamlfmt` — YAML validation and formatting

## Architecture Decisions

### Single Binary

The entire proxy is a single Go binary with no external runtime dependencies.
This simplifies distribution and deployment.

### Statically Linked

`CGO_ENABLED=0` ensures the binary has no C library dependencies, making it
portable across Linux distributions and macOS versions.

### Version Subcommand

The `version` subcommand outputs JSON in the same format as `kubectl version`,
making it compatible with tooling that parses Kubernetes version information:

```json
{"gitVersion": "v1.0.0", "gitCommit": "abc1234"}
```

### TLS Support

Optional TLS is configured via `PROXY_CERT` and `PROXY_KEY` environment
variables. When both are set, the proxy listens with TLS 1.2+ and a curated
set of cipher suites. When absent, it listens on plain HTTP.

### Signal Handling

The proxy listens for `SIGINT` and `SIGTERM` for graceful shutdown, closing
the listener before exiting.

## Best Practices

1. **Run validation before committing:** `trunk check`
2. **Keep `go.mod` tidy:** run `make tidy` after dependency changes
3. **Test with real kubeconfig** before submitting PRs
4. **Follow existing patterns** — the codebase is intentionally minimal
5. **Use `gofmt`** for formatting (enforced by Trunk)
6. **Do not use Anthropic Fable for coding tasks** — Fable may be used only
   for planning, analysis, and thinking through problems. When writing or
   editing code, use standard editing tools instead.

## GitHub Actions (Claude Code Action) Rules

When operating via the `claude.yaml` workflow (i.e., invoked from a PR
comment, issue, or review), follow these rules:

### Code Review

When reviewing code and proposing fixes:

1. **Show the diff first** — present every proposed change as a unified diff
   block using the `diff` language tag:

   ```diff
   --- a/main.go
   +++ b/main.go
   @@ -10,7 +10,7 @@
    const oldLine = "before";
   -const changedLine = "after";
   +const changedLine = "the fix";
    const unchangedLine = "same";
   ```

   You can generate this from the terminal with:
   ```bash
   git diff -u -- main.go
   ```

   If the change spans multiple files, group them under a single commit
   subject and show each file's diff sequentially.

2. **Propose a commit subject first** — before any code change, output a
   single line with the proposed commit subject:

   ```text
   **Proposed commit:** <short description>
   ```

   Do **not** use Conventional Commits prefixes (e.g. `fix:`, `feat:`,
   `chore:`, `refactor:`, `docs:`, `test:`, `ci:`). This project prefers
   plain, descriptive commit messages and PR titles without any prefix.

   Wait for the user to confirm (or adjust) the subject before applying the
   change.

3. **Comment style:**
   - Keep review comments concise and actionable
   - Reference specific lines (file + line number) when pointing out issues
   - Offer a concrete fix suggestion rather than just flagging a problem
   - Do **not** use emoji in any Markdown, comments, commit messages, or
     PR descriptions. The only exception is emoji that already appears
     inside code strings (e.g. application logs, user-facing messages).
   - Use GitHub's `suggestion` block for small targeted fixes so the PR
     author can accept the change with a single click:

     ````suggestion
     <same unified-diff format as shown above>
     ````

   - For larger multi-file changes, use `diff -u` blocks in a regular
     comment instead, with the proposed commit subject shown first

### Making Changes to a PR

When asked to implement a change on a PR:

1. Propose the commit subject (as above)
2. Describe what will change and why
3. After confirmation, apply the changes with commits on the PR branch
4. **One commit per fix** — when a review surfaces more than one issue or
   the plan includes more than one fix, apply and commit each fix
   separately. Do not batch multiple independent fixes into a single
   commit. This keeps the history bisectable and makes each change easy
   to revert individually.

### Branch Naming Conventions

When creating a branch from an issue, use a human-readable name that includes
the issue number and a short slug derived from the issue title:

```text
claude/issue-<number>-<short-slug>
```

- `<number>` is the GitHub issue number
- `<short-slug>` is a kebab-case summary of the issue title, kept short
  (3–6 words maximum, omit articles and filler words)
