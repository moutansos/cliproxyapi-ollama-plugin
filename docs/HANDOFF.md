# Handoff: cliproxyapi-ollama

This document covers the project's state, how it was verified, and how to
deploy and roll it back. Deployment-specific details (host names, addresses,
backup file names) are kept out of the repository.

## What was built

This is a native CLIProxyAPI v8.0.11 plugin (`cliproxyapi-ollama.so`, plugin ID
`cliproxyapi-ollama`, provider/owned_by `ollama`). It:

- discovers models on one Ollama instance with `/api/tags`, `/api/show` (cached
  by digest), and `/api/ps`;
- serves them through native `/api/chat` with streaming and non-streaming
  responses, tools, data-URL images, thinking (`reasoning_effort` → `think`),
  usage, cancellation, and `http_status` errors;
- publishes `context_length`, `max_completion_tokens`, and `display_name` in
  `/v1/models`;
- supports per-model **observe/managed** context policies, editable as CPAMC
  plugin config fields.

There is also a read-only diagnostics route,
`GET /v0/management/plugins/cliproxyapi-ollama/status`, plus a `POST …/refresh` route.

See `README.md` (usage) and `docs/design.md` (architecture and v8.0.11
findings).

## Key design decisions

- **Live refresh.** CLIProxyAPI v8.0.11 does not periodically republish plugin
  static models. The plugin therefore uses:
  - a **ModelRouter** that claims every ID in its snapshot or prefix namespace
    and executes it on the plugin's own executor with no auth;
  - a **ResponseInterceptor** that rewrites the OpenAI `/v1/models` list from
    the same live snapshot. The stock handler reduces each entry to `id`,
    `object`, `created`, and `owned_by`.

  Listing and routing share one immutable snapshot.
- **Observe** (the default) injects nothing. Its context source order is
  observed (`/api/ps`) → configured (`num_ctx` from `/api/show`) → fallback →
  unknown (omitted). The architecture maximum is never advertised as an
  allocation.
- **Managed** injects `num_ctx` (capped to the declared maximum),
  `num_predict` as a ceiling (a smaller client value is kept), and
  `keep_alive`. It then verifies with `/api/ps` and advertises a smaller
  allocation with a discrepancy note if Ollama did not honor `num_ctx`.
- **`model_prefix`** follows CLIProxyAPI's convention: a bare name becomes
  `<prefix>/<name:tag>`. The default `display_name` is the public ID, so
  clients that show display names keep the prefix.
- **Bad config never unloads the provider.** Fatal validation issues keep the
  last valid config, and per-model issues drop only that override.

## Verification

- **Unit tests:** `go vet ./...` and `go test -race ./...` use a fake Ollama.
  No live Ollama is needed.
- **Local end-to-end test:** the `eceasy/cli-proxy-api:v8.0.11` image with the
  built `.so` and a fake Ollama covered:
  - OpenAI streaming and non-streaming, Claude Messages, Responses, and tool
    calls;
  - 404 and 502 errors on both paths;
  - outage retention, and live refresh of a newly added model;
  - config rejection, client disconnects, and CPAMC config fields.
- **Live deployment** (CLIProxyAPI v8.0.11, Ollama v0.35.0):
  - Observe mode advertised `num_ctx`-derived context and omitted unknown
    context.
  - A model that Ollama's own `/api/show` cannot read was excluded with a
    reason.
  - A managed override changed the advertised context immediately; `/api/ps`
    confirmed the allocation (`verified: true`).
  - Streaming and non-streaming chat worked through CLIProxyAPI.
  - OpenCode V2 (through `opencode2-cliproxyapi` ≥ 0.3.0) reported the
    advertised limits.

## Artifact

Releases follow the CLIProxyAPI plugin store format:

- one `cliproxyapi-ollama_<version>_<goos>_<goarch>.zip` per platform, with the
  library at the zip root;
- a `checksums.txt` in `sha256sum` format.

The platforms are linux amd64/arm64, darwin arm64, and windows amd64.
darwin/amd64 is not built: Go uses one fixed TLS slot (`%gs:0x30`) for the
goroutine pointer there, which a Go plugin shares with the Go-based host. The
plugin crashed CLIProxyAPI on its first call in CI (`fatal error: unknown
caller pc`; see golang/go#38692). The store review asks for darwin/amd64; see
`docs/store-submission.md`.

The `Build` workflow runs on every push and pull request. For each platform it:

- builds the library (Linux in manylinux2014, failing above glibc 2.17;
  macOS and Windows natively);
- packages it and verifies the archive against the store installer's rules
  (`internal/release`);
- runs `tools/smoke`, which loads the library into the matching CLIProxyAPI
  v8.0.11 release binary against a fake Ollama and checks discovery, listing,
  chat (OpenAI and Claude, streaming and not), managed context, and the
  authenticated diagnostics routes.

A `v<version>` tag that matches `VERSION` also publishes the GitHub release.

`make build`, `make package`, and `make smoke` run the same Linux steps
locally. The store submission is prepared in `docs/store-submission.md`.

## Deployment procedure (Docker)

`$PLUGINS` is the host directory mounted at `/CLIProxyAPI/plugins`, and
`$CONFIG` is the host `config.yaml`.

1. Back up the config on the host, keeping owner and mode:

   ```sh
   cp -p "$CONFIG" "$CONFIG.bak-ollama-$(date -u +%Y%m%dT%H%M%SZ)"
   ```

2. Install the library:

   ```sh
   install -m 0755 cliproxyapi-ollama.so "$PLUGINS/linux/amd64/cliproxyapi-ollama.so"
   ```

3. Enable and configure it through CPAMC, or with
   `PUT /v0/management/plugins/cliproxyapi-ollama/config` and body
   `{"enabled": true, "priority": 10, "base_url": "http://<ollama-host>:11434"}`.
   CLIProxyAPI reloads without a restart.
4. Check `GET /v0/management/plugins/cliproxyapi-ollama/status` and `/v1/models`.

**Updating** an installed library at the same path takes effect only after a
CLIProxyAPI restart. Go `c-shared` libraries cannot be unloaded, and a
same-path reload only re-runs `reconfigure`. Keep the previous `.so` outside
the plugins directory as a rollback copy.

## Rollback

1. **Disable** (no restart; either option):
   - In CPAMC, go to Plugins → `cliproxyapi-ollama` and toggle it off.
   - Or call the management API:

     ```sh
     PATCH /v0/management/plugins/cliproxyapi-ollama/enabled  {"enabled": false}
     ```

2. **Restore the config backup** with `cp -p`. CLIProxyAPI's file watcher
   reloads it.
3. **Return to a previous build:** reinstall the saved `.so` and restart
   CLIProxyAPI.
4. **Remove the library:** delete it from the plugins directory. Loaded code
   stays mapped until the next restart, but it is inert once disabled.

## Limitations

- **Manual updates need a restart:** a manually installed `.so` overwritten at
  the same path takes effect only on a CLIProxyAPI restart. Store installs use
  versioned file names and update in place.
- **Lagging views:** the Claude-format `/v1/models` and CPAMC model pickers are
  registry-backed and update only on config reload. The OpenAI `/v1/models`
  list and routing are live.
- **Unknown context:** a model that is not loaded and has no `num_ctx`
  advertises no context unless `fallback_context_length` is set.
- **Observed values and other clients:** an observed value reflects whoever
  loaded the runner.
- **Unsupported (501/400):** `count_tokens`, remote image URLs, `n > 1`, audio,
  and enforced `tool_choice`.
- **Interceptor overhead:** every successful non-streaming response passes
  through the response interceptor, which runs a few cheap field checks.

## Suggested follow-ups

- A rich CPAMC per-model editor: a table of discovered models with their
  policy, source, and verification. Resource routes in v8.0.11 are not
  management-authenticated, so this needs an authenticated design.
- Multi-instance support: an `instances` list, per-instance prefixes, and
  overrides keyed `<instance>/<name:tag>`.
