# Plugin store submission

Prepared registry change for
[router-for-me/CLIProxyAPI-Plugins-Store](https://github.com/router-for-me/CLIProxyAPI-Plugins-Store).
The pull request is **not opened**. The branch is
`add-cliproxyapi-ollama` on the
[moutansos/CLIProxyAPI-Plugins-Store](https://github.com/moutansos/CLIProxyAPI-Plugins-Store)
fork, one commit that appends a single entry to `registry.json`.

## Registry entry

```json
{
  "id": "cliproxyapi-ollama",
  "name": "Ollama Provider",
  "description": "Serves the models of an Ollama instance through CLIProxyAPI, publishing deployment-accurate context_length, max_completion_tokens, and display_name in /v1/models. Each model can observe Ollama's real allocation or have num_ctx, num_predict, and keep_alive managed from CPAMC. 将 Ollama 实例的模型接入 CLIProxyAPI，在 /v1/models 中发布与实际部署一致的 context_length、max_completion_tokens 和 display_name，每个模型可观察 Ollama 的真实上下文分配，或由 CPAMC 统一管理 num_ctx、num_predict 和 keep_alive。",
  "author": "moutansos",
  "repository": "https://github.com/moutansos/cliproxyapi-ollama-plugin",
  "homepage": "https://github.com/moutansos/cliproxyapi-ollama-plugin",
  "license": "MIT",
  "tags": ["Provider", "Executor", "Model Router", "Ollama", "Local Models"]
}
```

The `id` is unused in the store. The entry has no `version` field, so CPAMC shows
the latest GitHub release, and no `logo`.

## Validation already done

- **CLIProxyAPI's own installer, against the live release.** A scratch test in
  a throwaway checkout of CLIProxyAPI v8.0.11 called `Client.Install` for the
  published `v0.1.1` release. It downloaded each archive, verified
  `checksums.txt`, and installed the library under its versioned name:
  `linux/amd64/cliproxyapi-ollama-v0.1.1.so`,
  `linux/arm64/cliproxyapi-ollama-v0.1.1.so`,
  `darwin/arm64/cliproxyapi-ollama-v0.1.1.dylib`, and
  `windows/amd64/cliproxyapi-ollama-v0.1.1.dll`. For `darwin/amd64` it reported
  the archive missing, which is the intended failure.
- **The registry parser.** The official `registry.json` with this entry
  appended parses with `pluginstore.ParseRegistry`, and `ValidatePlugin`
  accepts the entry (112 plugins, install type `github-release`).
- **A full store install in a real CLIProxyAPI v8.0.11 container.** With a
  local registry served as an extra store source, the management store listing
  showed the plugin as installed at `0.1.0` with an update available.
  `POST /v0/management/plugin-store/cliproxyapi-ollama/install` downloaded the
  release from GitHub, installed it, and hot-reloaded it (`restart_required:
  false`). The existing `base_url` and `model_prefix` were kept, the versioned
  library took over, and chat still worked. The old unversioned `.so` stayed
  on disk but is no longer loaded.
- **Per-platform smoke tests.** The
  [v0.1.1 release run](https://github.com/moutansos/cliproxyapi-ollama-plugin/actions/runs/37579810426)
  loads each build into the matching CLIProxyAPI v8.0.11 release binary and
  exercises discovery, listing, chat, managed context, and the diagnostics
  routes. All four passed.

## The darwin/amd64 gap

The store review asks for five platforms, and this release has four. darwin/amd64
is not shipped because a Go c-shared plugin cannot run inside the Go-based
CLIProxyAPI host on Intel macOS: Go stores the goroutine pointer in one fixed
TLS slot (`%gs:0x30`), so the plugin runtime and the host runtime share it. In
[CI](https://github.com/moutansos/cliproxyapi-ollama-plugin/actions/runs/37578357625)
the plugin crashed CLIProxyAPI on its first call. See
[golang/go#38692](https://github.com/golang/go/issues/38692) and the
"Not supported" section of the README.

## Draft pull request body

**Title:** Add cliproxyapi-ollama: Ollama provider with deployment-accurate context limits (v0.1.1)

```markdown
## Add cliproxyapi-ollama (Ollama provider)

- **Repository:** https://github.com/moutansos/cliproxyapi-ollama-plugin
- **Latest release:** [v0.1.1](https://github.com/moutansos/cliproxyapi-ollama-plugin/releases/tag/v0.1.1)
- **Plugin ID:** `cliproxyapi-ollama`. It is not used by any existing entry.
- **License:** MIT
- **Scope:** this PR changes only `registry.json`. It appends one entry and keeps every existing entry as is.

### Capability

A provider for an Ollama instance. CLIProxyAPI owns model selection, and the
plugin talks to Ollama's native `/api/chat`.

- **Discovery:** models come from `/api/tags`, `/api/show`, and `/api/ps`, and
  refresh on an interval. Embedding-only models are not advertised.
- **Context limits:** `/v1/models` carries `context_length`,
  `max_completion_tokens`, and `display_name` that match the deployment. Each
  model either observes the context Ollama actually loaded, or has `num_ctx`,
  `num_predict`, and `keep_alive` managed from CPAMC.
- **APIs:** OpenAI Chat Completions, OpenAI Responses, and Anthropic Messages,
  with streaming, tools, images, and thinking, through CLIProxyAPI's
  translators.
- **Read-only toward Ollama:** the plugin never pulls, creates, copies,
  deletes, or pushes models.

### Routes

There are no resource routes. Both management routes require the management key.

| Route | Auth | Purpose |
| --- | --- | --- |
| `GET /v0/management/plugins/cliproxyapi-ollama/status` | key | Discovery state, the context source per model, and managed verification. |
| `POST /v0/management/plugins/cliproxyapi-ollama/refresh` | key | Run discovery now and return the status. |

### Platform matrix

| Target | Release ZIP | Library at archive root |
| --- | --- | --- |
| darwin_arm64 | [Download](https://github.com/moutansos/cliproxyapi-ollama-plugin/releases/download/v0.1.1/cliproxyapi-ollama_0.1.1_darwin_arm64.zip) | `cliproxyapi-ollama.dylib` |
| linux_amd64 | [Download](https://github.com/moutansos/cliproxyapi-ollama-plugin/releases/download/v0.1.1/cliproxyapi-ollama_0.1.1_linux_amd64.zip) | `cliproxyapi-ollama.so` |
| linux_arm64 | [Download](https://github.com/moutansos/cliproxyapi-ollama-plugin/releases/download/v0.1.1/cliproxyapi-ollama_0.1.1_linux_arm64.zip) | `cliproxyapi-ollama.so` |
| windows_amd64 | [Download](https://github.com/moutansos/cliproxyapi-ollama-plugin/releases/download/v0.1.1/cliproxyapi-ollama_0.1.1_windows_amd64.zip) | `cliproxyapi-ollama.dll` |

[checksums.txt](https://github.com/moutansos/cliproxyapi-ollama-plugin/releases/download/v0.1.1/checksums.txt) has a SHA-256 entry for each archive.

**darwin_amd64 is intentionally not shipped.** On Intel macOS, Go stores the
goroutine pointer in one fixed TLS slot (`%gs:0x30`,
[golang/go#23617](https://github.com/golang/go/issues/23617)). A Go c-shared
plugin and the Go-based CLIProxyAPI host share that slot, so the plugin reads
the host's goroutine as its own and crashes the host on its first call. This
was reproduced in CI with the plugin loaded into the official CLIProxyAPI
v8.0.11 darwin/amd64 binary
([run](https://github.com/moutansos/cliproxyapi-ollama-plugin/actions/runs/37578357625)):
`fatal error: unknown caller pc`. The plugin's c-shared signal handlers
(SIGURG, SIGSEGV, SIGPIPE) would also see the host's goroutines. Shipping the
binary would crash every Intel Mac install, so the release omits it and the
store installer reports the asset as missing there.
[golang/go#38692](https://github.com/golang/go/issues/38692) tracks the
underlying limitation: `-buildmode=c-shared` is not supported for a Go library
opened by a Go program on Darwin. The other platforms are sound, because
darwin/arm64 uses a per-runtime pthread key and Linux uses per-image ELF TLS.

### Validation evidence

- **Release workflow:** [v0.1.1 release run](https://github.com/moutansos/cliproxyapi-ollama-plugin/actions/runs/37579810426).
  - `go vet` and `go test -race` run first.
  - Each platform builds the library, packages it, and verifies the archive
    against the v8.0.11 store installer's rules: asset name, `checksums.txt`,
    the library at the zip root, and the binary format and CPU of the library.
  - Linux builds run in manylinux2014 and fail if the library needs glibc
    newer than 2.17, the same baseline as CLIProxyAPI's own binaries. The
    published libraries need GLIBC_2.3.2 (amd64) and GLIBC_2.17 (arm64).
  - The darwin build targets macOS 12.
- **Load checks, all inside the official CLIProxyAPI v8.0.11 release binary
  for that platform, against a fake Ollama:**
  - **linux_amd64, linux_arm64, darwin_arm64, windows_amd64:** the CI smoke
    test checks plugin registration and CPAMC config fields, `/v1/models`
    metadata and embedding exclusion, non-streaming and streaming chat,
    Anthropic Messages, a 404 for an unknown model, a managed context override
    applied through the management API and verified against the backend, and
    the authenticated diagnostics route (which rejects a request without the
    management key). All four passed.
- **The store installer itself:** CLIProxyAPI v8.0.11's
  `internal/pluginstore.Client.Install` was run against the published release
  for all four platforms. Each archive was downloaded, checked against
  `checksums.txt`, and installed under its versioned file name.
- **A live store install:** a CLIProxyAPI v8.0.11 container with this registry
  entry served as an extra store source installed the plugin through
  `POST /v0/management/plugin-store/cliproxyapi-ollama/install`, hot-reloaded
  it without a restart, kept the existing configuration, and served chat.
```
