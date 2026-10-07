# Design: CLIProxyAPI Ollama provider plugin

Status: implemented (v0.1.0). Target host: CLIProxyAPI **v8.0.11**
(`eceasy/cli-proxy-api:v8.0.11`, commit `e2bff01`). Every ABI claim below was
checked against tag `v8.0.11` of `router-for-me/CLIProxyAPI`. File references
point at that tag.

## Goals

- Provide one native plugin (`cliproxyapi-ollama.so`, plugin ID
  `cliproxyapi-ollama`) that discovers models on one Ollama instance and serves
  them through CLIProxyAPI.
- Publish context limits that match the deployment, with **observe** as the
  default policy.
- Offer optional per-model **managed** `num_ctx` / `num_predict` / `keep_alive`,
  configured centrally in `plugins.configs.cliproxyapi-ollama` and editable in
  CPAMC.

## v8.0.11 findings

### ABI and registration

- The C ABI is unchanged from v7: `cliproxy_plugin_init`, plus a `call`,
  `free_buffer`, and `shutdown` table (`sdk/pluginabi.ABIVersion = 1`). The RPC
  JSON schema version is 6 (`sdk/pluginabi/types.go`).
- The plugin ID comes from the file name (`cliproxyapi-ollama.so`). Files are
  searched in `<plugins.dir>` and `<plugins.dir>/linux/amd64`.
- A plugin is loaded only when both `plugins.enabled` and
  `plugins.configs.<id>.enabled` are true (`internal/pluginhost/config.go`).
  Copying the `.so` alone does nothing.
- Registration requires non-empty `Name`, `Version`, `Author`, and
  `GitHubRepository` (`host.go validPlugin`). If register or reconfigure
  returns an error, the plugin record is dropped. **This plugin therefore never
  fails registration because of bad config**: it rejects the config, keeps the
  last valid one, and logs the problem (see Configuration).
- `plugin.reconfigure` runs on **every** config reload, whether triggered by
  CPAMC or a file edit. It is followed by `model.static` (`RegisterModels`) and
  executor registration (`sdk/cliproxy/service_config.go`,
  `service_plugins.go`).
- The host renders `Metadata.ConfigFields` in CPAMC
  (`internal/api/handlers/management/plugins.go ListPlugins`). It supports
  string, number, integer, boolean, enum, array, and object fields.

### Live refresh checkpoint

**v8.0.11 does not periodically republish plugin static models.**
`model.static` is called only at startup and after configuration changes
(`syncPluginModelRuntime` callers: `service_lifecycle.go`,
`service_config.go`). The periodic `registry.SetModelRefreshCallback` covers
only built-in providers that have auths.

Static models are also not enough to execute: a plugin executor with static
models still needs a provider auth on the normal conductor path.

The plugin solves both problems itself, using one catalog snapshot:

1. **ModelRouter** (`model.route`). The host calls this before model→provider
   resolution and auth selection for every request
   (`sdk/api/handlers/handlers_routing.go applyModelRouter`). The plugin claims
   every ID that is in the snapshot or inside its prefix namespace, and returns
   `TargetKind=self`. The host then calls this plugin's executor with no auth
   (`internal/pluginhost/executor_route.go`). Newly installed models route on
   the next refresh, or immediately through an on-demand refresh when an
   unknown prefixed ID is requested. Removed models get a clear 404.
2. **ResponseInterceptor** on `/v1/models`. See Model-list metadata.
3. **`model.static`** still publishes the snapshot, so the models also appear
   in registry-backed views (the Claude-format model list and CPAMC model
   pickers). Those views can be stale until the next config reload. The
   OpenAI-format `/v1/models` list is always live.

Listing and routing read the same `atomic.Pointer[catalog.Snapshot]`. An
in-flight request keeps the immutable snapshot it started with.

### Model-list interception

The stock OpenAI `/v1/models` handler still reduces each record to `id`,
`object`, `created`, and `owned_by` (`sdk/api/handlers/openai/openai_handlers.go
OpenAIModels`). It then passes the body to `WriteModelListResponse`, which calls
response interceptors with `Model == ""` and `SourceFormat == "openai"`
(`sdk/api/handlers/handlers_interceptors.go`).

The plugin declares `response_interceptor` and rewrites only bodies that
satisfy all of the following:

- The source format is `openai`.
- The model and requested model are empty.
- There is no request body.
- The status is 200.
- The JSON shape is `{"object":"list","data":[...]}`.

It removes entries with `owned_by == "ollama"` whose ID is in the snapshot or
the prefix namespace. It then inserts snapshot entries that carry
`display_name`, `context_length` (omitted when unknown), and
`max_completion_tokens` (when known). Other providers' entries keep their raw
JSON bytes and their order. Every other response gets an empty interceptor
reply, which the host treats as "unchanged".

Cost: the host sends every successful non-streaming response body across the
ABI to the interceptor. The plugin returns after a few field checks.

### Executor formats and streaming

- The plugin declares input and output format `chat-completions`. The host
  translates Claude Messages, Responses, and other clients to and from OpenAI
  chat (`adapters_executors.go prepareExecutorCall`). Claude, Responses, and
  OpenAI clients all work (verified end to end).
- Stream frame shape depends on the client:
  - When the client protocol is OpenAI chat, the host passes chunks through
    unchanged and adds `data: ` itself, so the plugin emits bare JSON.
  - For any other protocol, the host's OpenAI stream translators expect SSE
    `data: {...}` lines.

  The executor request reports the translated formats but not the client's
  format, so the plugin uses the `request_path` metadata to decide. It falls
  back to comparing `OriginalRequest` with `Payload`.
- `executor.execute_stream` opens the upstream stream **synchronously**. A
  non-2xx Ollama response becomes an error envelope with `http_status`, just as
  on the non-stream path. Chunks are then emitted asynchronously with
  `host.stream.emit` and finished with `host.stream.close`. Errors after
  headers are sent close the stream with a message; HTTP status can no longer
  change at that point.
- Cancellation: requests carry the executor's `host_callback_id`, so the host
  cancels the upstream HTTP call when the client goes away. If
  `host.stream.emit` fails, the plugin also closes the upstream stream right
  away, so Ollama stops generating.
- Upstream HTTP always goes through the host callbacks `host.http.do` and
  `host.http.do_stream`, so it follows host transport policy and request
  logging. Background discovery has no request context. It uses
  `host.http.operation_open` and `host.http.cancel` to enforce its own
  timeouts.
- Error mapping:

  | Condition | `http_status` |
  | --- | --- |
  | Invalid request | 400 |
  | Unknown model | 404 |
  | Ollama 404 | 404 |
  | Ollama 429 | 429 |
  | Ollama 5xx or unreachable | 502 |
  | Ollama 401/403 (gateway credentials) | 502 |
  | Discovery never succeeded | 503 |

  In v8.0.11 the host serializes any status ≥ 500 as `server_error`, while
  keeping the plugin's message.

### Simpler alternatives considered

- **Proxying Ollama's OpenAI-compatible `/v1/chat/completions`** would avoid
  writing a translator. It was rejected because that endpoint cannot carry
  `num_ctx` or `keep_alive`, so managed mode would not be enforceable. The
  native `/api/chat` endpoint is required.
- **CPA's built-in `openai-compatibility` provider pointed at Ollama** has the
  same limitation and cannot publish deployment-accurate context.

## Components

| Package | Responsibility |
| --- | --- |
| `internal/config` | Parse and validate plugin YAML; per-model overrides; inheritance (`EffectiveFor`). |
| `internal/ollama` | Native API types, and parsing for `/api/show` parameters and architecture context. Read-only client: `tags`, `show`, `ps`. |
| `internal/upstream` | Transport interface: CPA host callbacks in production, `net/http` in tests. |
| `internal/catalog` | Discovery with a digest-keyed `/api/show` cache and outage retention; context resolution; immutable snapshots. |
| `internal/translate` | OpenAI chat ⇄ `/api/chat`, including tools, images, thinking, NDJSON→chunk streaming, and usage. |
| `internal/modellist` | `/v1/models` enrichment. |
| `internal/service` | Lifecycle, refresh loop, router, executor, interceptor, management diagnostics. |
| `cmd/cliproxyapi-ollama` | cgo exports, RPC dispatch, host-callback bridge. |

## Discovery

- `/api/tags` runs on every refresh, every `refresh_interval_seconds` (default
  60).
- `/api/show` runs only for included models with a new or changed digest.
  Successful results are cached by `name@digest`; failures are retried on the
  next pass.
- `/api/ps` runs on every refresh and after managed requests.
- A model is served if its capabilities include `completion`. Embedding-only
  models are excluded. When Ollama reports no capabilities, a `bert` or `embed`
  heuristic excludes embedders.
- If `/api/show` fails and no earlier result exists for that digest, the model
  is excluded with the reason shown in diagnostics. Example: on the live server,
  `gpt-oss:20b` fails `/api/show` with "unsupported tensor … size overflows".
  Ollama itself cannot read it, so it is not advertised.
- If `/api/tags` fails, the previous facts and snapshot are kept, and the error
  is recorded and logged once per state change.
- The plugin never calls pull, create, copy, delete, push, or blob endpoints.
  Tests assert this.

## Context policy

Each model's mode is `inherit | observe | managed`; the instance default is
`observe`.

**Observe** injects no runtime context settings. It advertises:

1. `observed`: `/api/ps` `context_length` while the model is loaded.
2. `configured`: `num_ctx` from `/api/show` parameters, capped to the declared
   maximum (Ollama clamps to it too).
3. `fallback`: `fallback_context_length` (global or per model), capped.
4. `unknown`: `context_length` is omitted.

The architecture or declared maximum is never advertised as an allocation. It
is shown in diagnostics as `declared_max_context` alongside
`observed_at`/`context_source`.

Caveat: an observed value reflects whoever loaded the runner. If another client
(such as Open WebUI) loaded it with a different `num_ctx`, Ollama may reload for
an observe-mode request.

**Managed** injects these values on this plugin's requests only:

- `options.num_ctx`: the configured value, capped to the declared maximum.
- `options.num_predict`: a ceiling. The client's `max_tokens` /
  `max_completion_tokens` is kept when it is smaller, and the ceiling is used
  when the client sends none.
- `keep_alive`: `-1` is sent as a number, durations as strings.

The advertised context is the managed `num_ctx`. After a managed request (when
`verify_managed_allocation` is true), `/api/ps` is checked in the background:

- If Ollama loaded a **smaller** context, the smaller value is advertised and a
  `discrepancy` is recorded.
- If the model was not loaded at check time, a discrepancy note is recorded.

Ollama failures are passed to the client with a mapped status (for example
"model requires more system memory" → 502).

Switching back to observe stops injection for later requests. It does not
unload runners.

## Configuration

`model_prefix` follows CLIProxyAPI's `normalizeModelPrefix`
(`internal/config/config_normalization.go`): the value is trimmed of spaces and
slashes, and IDs are `<prefix>/<name:tag>`. An internal `/` is rejected, and an
empty value means no prefix (only advertised IDs are claimed).

The config lives under `plugins.configs.cliproxyapi-ollama`; the README has the
field reference. Instance fields are grouped in `config.Instance` with an `ID`,
so multiple instances can be added later without changing per-model override
keys. Overrides are keyed by the exact upstream `name:tag` of the single v1
instance.

Validation has two levels:

- **Fatal issues** reject the whole config. Examples: a bad URL, prefix, or
  interval; a non-object `models` value; malformed YAML. The previous valid
  config stays active and an error is logged.
- **Per-model issues** drop only that override, so the model uses the default
  policy. Examples: an unknown field, a bad policy, `num_ctx` outside
  256–16 777 216, or managed fields on an observe override. A managed policy
  without any `num_ctx` falls back to observe.

All issues appear at `GET /v0/management/plugins/cliproxyapi-ollama/status`.

CPAMC object and array fields are accepted either as YAML collections or as
JSON strings.

## Diagnostics

These routes require the management key:

- `GET /v0/management/plugins/cliproxyapi-ollama/status`: config summary (with the API
  key redacted to a boolean), discovery state, per-model resolution with
  source, observation time, declared max, verification and discrepancy, the
  excluded models with reasons, config issues, and overrides that match no
  model.
- `POST /v0/management/plugins/cliproxyapi-ollama/refresh`: run discovery now.

The plugin registers no unauthenticated resource routes.

## Known limitations

- Registry-backed lists (the Claude-format `/v1/models` and CPAMC model views)
  update only on config reload. The OpenAI `/v1/models` list and routing are
  live.
- `count_tokens` and raw `http_request` return 501.
- Only base64 `data:` image URLs are accepted; remote image URLs return 400.
- `tool_choice` values other than `none` are not enforceable on Ollama and are
  ignored.
- `n > 1`, logprobs, and audio are not supported.
- There is one instance in v1.
