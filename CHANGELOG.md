# Changelog

All notable changes to this project are documented here. The format is based
on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project
follows [Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.1.1] - 2026-10-06

### Added

- Releases are now in the CLIProxyAPI plugin store format. Each release has one
  `cliproxyapi-ollama_<version>_<goos>_<goarch>.zip` per platform, with the
  library at the zip root, plus a `checksums.txt` in `sha256sum` format.
- Builds for Linux (amd64, arm64), macOS on Apple silicon (arm64), and Windows
  (amd64).
  - CI loads each build into the matching CLIProxyAPI v8.0.11 release binary
    and runs a smoke test against a fake Ollama.
  - Linux libraries are built on manylinux2014, so they need at most glibc
    2.17, the same baseline as CLIProxyAPI's own binaries.

### Not supported

- Intel macOS (darwin/amd64). Go hard-codes the goroutine pointer at
  `%gs:0x30` there, so a Go plugin's runtime and the Go-based CLIProxyAPI host
  share one TLS slot, and the plugin crashes the host on its first call
  ([golang/go#38692](https://github.com/golang/go/issues/38692)).

### Changed

- The diagnostics routes moved to `GET /v0/management/plugins/cliproxyapi-ollama/status`
  and `POST /v0/management/plugins/cliproxyapi-ollama/refresh`, under the
  plugin namespace other store plugins use. They still require the
  management key.
- The default `display_name` is the public model ID (for example
  `ollama/qwen3:8b`), so clients that show display names keep the configured
  prefix.

## [0.1.0] - 2026-10-04

### Added

- First release, a native CLIProxyAPI v8.0.11 plugin.
  - It discovers Ollama models with `/api/tags`, `/api/show`, and `/api/ps`,
    and serves them through native `/api/chat`.
  - It publishes `context_length`, `max_completion_tokens`, and
    `display_name` in `/v1/models`.
  - Each model has an observe or managed context policy, configured in CPAMC.
- This release shipped a bare linux/amd64 `.so`, not the plugin store format.

[Unreleased]: https://github.com/moutansos/cliproxyapi-ollama-plugin/compare/v0.1.1...HEAD
[0.1.1]: https://github.com/moutansos/cliproxyapi-ollama-plugin/compare/v0.1.0...v0.1.1
[0.1.0]: https://github.com/moutansos/cliproxyapi-ollama-plugin/releases/tag/v0.1.0
