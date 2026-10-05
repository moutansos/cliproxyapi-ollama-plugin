# CLIProxyAPI Ollama provider plugin

This is a native [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)
plugin for **v8.0.11**. It serves the models of an Ollama instance through
CLIProxyAPI. It:

- discovers models dynamically using `/api/tags`, `/api/show`, and `/api/ps`;
- routes requests to Ollama's native `/api/chat`, with streaming, tools,
  images, thinking, and usage reporting;
- publishes context limits that match the deployment in `/v1/models`
  (`context_length`, `max_completion_tokens`, `display_name`);
- has a per-model context policy: **observe** (the default) or **managed**,
  edited centrally in CPAMC.

Clients can use OpenAI Chat Completions, Responses, or Claude Messages; the
host translates between them. Model IDs have the form `<prefix>/<exact Ollama
name:tag>`, for example `ollama/qwen3-coder:30b`.

The plugin only reads model information from Ollama. It never pulls, creates,
copies, deletes, or pushes models.

See [`docs/design.md`](docs/design.md) for the architecture and the v8.0.11 ABI
findings.

## Build

```sh
make test      # go test ./...
make vet       # go vet ./...
make build     # build/plugins/linux/amd64/cliproxyapi-ollama.so (prints SHA-256)
```

`make build` compiles inside `golang:1.26-bookworm`, the same toolchain image
CLIProxyAPI v8.0.11 is built with. The container runs `debian:bookworm` with
glibc 2.36, and the artifact needs at most `GLIBC_2.34`. `make build-local`
uses the host toolchain instead. Use it only if your host's glibc is not newer
than the container's.

The plugin ID comes from the file name, so keep it as `cliproxyapi-ollama.so`.

## Install / update / remove

These steps are for a Docker deployment, where a host directory (`$PLUGINS`
below) is mounted at `/CLIProxyAPI/plugins`.

**Install**

1. Copy the library into a directory the plugin search covers (`<plugins.dir>`
   or `<plugins.dir>/linux/amd64`):

   ```sh
   sudo install -o root -g root -m 0755 cliproxyapi-ollama.so \
     "$PLUGINS/linux/amd64/cliproxyapi-ollama.so"
   ```

2. Enable and configure it, preferably in CPAMC under **Plugins →
   cliproxyapi-ollama**, or through the management API:

   ```sh
   curl -X PUT -H "Authorization: Bearer $MGMT_KEY" -H 'Content-Type: application/json' \
     http://<cpa-host>:8317/v0/management/plugins/cliproxyapi-ollama/config \
     -d '{"enabled": true, "priority": 10, "base_url": "http://<ollama-host>:11434"}'
   ```

   CLIProxyAPI reloads its config and loads the plugin without a restart.
   `plugins.enabled` must already be `true`.

**Update.** Overwrite the `.so` with the same name. Then trigger a reload,
for example by saving the plugin config in CPAMC, or restart CLIProxyAPI.
Note: Go `c-shared` libraries cannot be truly unloaded. A same-path update is
picked up reliably only after a CLIProxyAPI restart.

**Disable.** Set `enabled: false`, either with the CPAMC toggle or with
`PATCH /v0/management/plugins/cliproxyapi-ollama/enabled` and body
`{"enabled": false}`.

**Remove.** Disable the plugin, delete the `plugins.configs.cliproxyapi-ollama`
block (CPAMC's delete removes both the file and the config), then delete the
`.so`.

## Configuration

All fields are optional and appear in CPAMC. Object and array fields can be
YAML collections or JSON strings.

| Field | Type | Default | Meaning |
| --- | --- | --- | --- |
| `base_url` | string | `http://127.0.0.1:11434` | Ollama native API URL. Trailing `/v1` or `/api` is stripped. |
| `api_key` | string | – | Optional bearer token for an authenticating proxy in front of Ollama. Never shown in diagnostics. |
| `model_prefix` | string | `ollama` | Public ID prefix, following the CLIProxyAPI convention: give a bare name and the `/` is added (`local` → `local/<name:tag>`); surrounding slashes are ignored, and an internal `/` is rejected. Empty means no prefix. The plugin owns this namespace: requests in it are routed to Ollama (unknown models get 404). |
| `refresh_interval_seconds` | integer | 60 | Discovery interval (10–86400). |
| `request_timeout_seconds` | integer | 10 | Timeout for each discovery call (1–300). Inference has no plugin timeout. |
| `default_context_policy` | enum | `observe` | `observe` or `managed`. Used by models without an override or with `policy: inherit`. |
| `default_num_ctx` | integer | 0 | Managed `num_ctx` for models that inherit managed. |
| `default_num_predict` | integer | 0 | Managed output ceiling. |
| `default_keep_alive` | string | – | Managed `keep_alive` (`5m`, `1h`, `300` seconds, `-1` = keep loaded). |
| `fallback_context_length` | integer | 0 | Observe-mode value for a model that is not loaded and has no `num_ctx` parameter. Set it to your server's `OLLAMA_CONTEXT_LENGTH`. `0` leaves the context unknown, and `context_length` is omitted. |
| `verify_managed_allocation` | boolean | true | Check `/api/ps` after managed requests. |
| `include_models` | array | all | Glob patterns (`path.Match`) of Ollama names to expose. |
| `exclude_models` | array | none | Glob patterns to hide. |
| `models` | object | `{}` | Per-model overrides keyed by the **exact** Ollama `name:tag`. |

Per-model override fields:

| Field | Meaning |
| --- | --- |
| `policy` | `inherit` (default), `observe`, or `managed`. |
| `num_ctx` | Managed context window. Capped to the model's declared maximum. Required for managed, unless `default_num_ctx` is set. |
| `num_predict` | Managed output ceiling. |
| `keep_alive` | Managed keep-alive. |
| `fallback_context_length` | Observe-only per-model fallback. |
| `display_name` | Overrides the default name. By default it is the public ID (`<prefix>/<name:tag>`), or `<name:tag> (Ollama)` when there is no prefix. |

Example:

```yaml
plugins:
  enabled: true
  configs:
    cliproxyapi-ollama:
      enabled: true
      priority: 10
      base_url: http://ollama.lan:11434
      model_prefix: ollama
      fallback_context_length: 4096
      exclude_models: ["*embed*"]
      models:
        "qwen3.8-27b-32k:latest":
          policy: managed
          num_ctx: 16384
          num_predict: 4096
          keep_alive: 10m
```

Validation:

- An invalid global value, such as a bad URL or an out-of-range interval,
  **rejects the whole update**. The previous valid config stays active, so a
  typo cannot unload the provider.
- An invalid override is dropped, and that model uses the default policy.
- All issues are logged and listed in the diagnostics endpoint.

Saved changes apply to later requests. In-flight requests keep the settings
they started with.

## Observe and managed

**Observe** (default) sends nothing that changes Ollama's runtime allocation.
The advertised `context_length` is, in priority order:

1. **observed**: the `context_length` from `/api/ps` while the model is loaded;
2. **configured**: `num_ctx` baked into the model (`/api/show` parameters);
3. **fallback**: `fallback_context_length`;
4. **unknown**: the field is omitted.

The architecture maximum (for example `qwen3moe.context_length: 262144`) is
never advertised as the allocation. Diagnostics show it as
`declared_max_context`.

**Managed** injects these values, for that model only and only on requests made
through this plugin:

- `options.num_ctx`, capped to the declared maximum;
- `options.num_predict` as a **ceiling**: a smaller client `max_tokens` /
  `max_completion_tokens` is preserved;
- `keep_alive`, when configured.

The managed value is advertised. After a request, `/api/ps` is checked; if
Ollama allocated less, the smaller value is advertised and a discrepancy is
reported. If Ollama cannot serve the target (for example, not enough memory),
the request fails with Ollama's message. Switching a model back to observe stops
injection but does not unload runners.

Managed settings affect a shared Ollama: other clients of the same model
(Open WebUI) may trigger reloads when their `num_ctx` differs.

## CPAMC usage

Open `http://<cpa-host>:8317/management.html`, go to **Plugins**, and select
`cliproxyapi-ollama`. The fields above are shown with their descriptions;
`models`, `include_models`, and `exclude_models` are JSON textareas. To manage
one model, set `models` to:

```json
{"qwen3.8-27b-32k:latest": {"policy": "managed", "num_ctx": 8192, "keep_alive": "5m"}}
```

Save. `/v1/models` shows the new `context_length` immediately.

## Diagnostics

These routes require the management key:

```sh
curl -H "Authorization: Bearer $MGMT_KEY" http://<cpa-host>:8317/v0/management/cliproxyapi-ollama/status
curl -X POST -H "Authorization: Bearer $MGMT_KEY" http://<cpa-host>:8317/v0/management/cliproxyapi-ollama/refresh
```

The status response covers:

- per-model `context_source`, `observed_at`, `declared_max_context`,
  `configured_num_ctx`, managed values, and verification;
- excluded models with reasons (for example `embedding-only model`, or Ollama
  failing to read a model);
- discovery errors, config issues, and overrides that match no model.

## Behavior notes

- **Live refresh.** CLIProxyAPI re-reads plugin static models only on startup
  and on config reload. This plugin therefore routes its own namespace with a
  ModelRouter and rewrites the OpenAI `/v1/models` list from its live snapshot:
  new Ollama models appear and route within one refresh interval (or
  immediately on first request), and removed ones disappear. The Claude-format
  model list and CPAMC model pickers use the host registry and can lag until
  the next config save.
- **Outages.** When Ollama is unreachable, the last successful catalog stays
  advertised. Requests then fail with 502.
- **Not supported** (returns 501 or 400): `count_tokens`, remote (non-`data:`)
  image URLs, `n > 1`, and audio.
