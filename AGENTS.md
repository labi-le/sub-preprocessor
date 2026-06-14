# Agent Instructions

## Running commands

Always run project commands via `nix-shell` — the toolchain (Go version, linter, etc.)
is defined in `shell.nix`. Running tools directly may use different versions or fail.

```bash
# Test
nix-shell --run "go test ./..."

# Format
nix-shell --run "go fmt ./..."

# Lint (config: .golangci.yml)
nix-shell --run "golangci-lint run"

# Launch the app (config: ./config.yaml)
nix-shell --run "make run"
```

## Project layout

- `main.go` — entry point
- `config.yaml` — application configuration
- `Makefile` — common targets (`run`, `test`, `fmt`)
- `.golangci.yml` — linter configuration
- `internal/` — internal packages
