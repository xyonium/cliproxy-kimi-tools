# cliproxy-kimi-tools

A native plugin for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)
that forwards datasource-tool requests to the Kimi Datasource upstream at
`https://api.kimi.com/coding/v1/tools`. It registers a Management API route
under **`/v0/management/kimi/tools`**.

The upstream token is read fresh from a cliproxy kimi auth JSON on every
request (mtime-cached). cliproxy's keeper keeps the auth file live; this
plugin is a pure reader and never writes it.

## How it works

| Route | Purpose |
|---|---|
| `POST /v0/management/kimi/tools` | Forward an arbitrary Kimi datasource payload upstream. Injects `Authorization: Bearer <access_token>` + `X-Msh-Device-Id: <device_id>` from the auth file; relays the response verbatim. |

The inbound `Authorization` header (which carries the CLIProxyAPI management
key — already validated by the host middleware) is **never** relayed upstream.

On a 401, the plugin re-reads the auth file once (cliproxy's keeper may have
just rotated it) and retries the upstream call a single time.

## Installation

Build the shared library:

```shell
make build            # linux/amd64 → dist/linux/amd64/kimi-tools.so
make build GOOS=darwin GOARCH=arm64
```

Or use `go build` directly:

```shell
go build -buildmode=c-shared -o kimi-tools.so .
```

Place `kimi-tools.so` (Linux) / `kimi-tools.dylib` (macOS) / `kimi-tools.dll`
(Windows) in your CLIProxyAPI plugins directory (e.g.
`plugins/linux/amd64/kimi-tools.so`).

## Configuration

In `config.yaml`:

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    kimi-tools:
      enabled: true
      # Glob for cliproxy kimi auth JSON. First enabled file wins.
      kimi_auth_file: /CLIProxyAPI/auths/kimi-*.json
      # Optional override; default shown.
      upstream_base_url: https://api.kimi.com/coding/v1/tools
```

| Field | Type | Required | Description |
|---|---|---|---|
| `kimi_auth_file` | string | yes | Glob for the cliproxy kimi auth JSON (e.g. `/CLIProxyAPI/auths/kimi-*.json`). The first file that exists, parses, and has `disabled != true` is used; the plugin reads `access_token` and `device_id` from it per request (mtime-cached). |
| `upstream_base_url` | string | no | Kimi upstream endpoint. Default `https://api.kimi.com/coding/v1/tools`. |

Reload or restart CLIProxyAPI after changing the config header; the plugin
receives the new YAML via `plugin.reconfigure`.

## Client usage

The endpoint lives under `/v0/management`, which the host guards with the
management key. So **the client must send the CLIProxyAPI management key**
on every call. Use either `Authorization: Bearer …` or `X-Management-Key: …`.

```python
import httpx

resp = httpx.post(
    "http://localhost:8317/v0/management/kimi/tools",
    headers={
        "Authorization": "Bearer <MANAGEMENT_KEY>",
        "Content-Type": "application/json",
    },
    json={"method": "list_data_sources", "params": {}},
)
print(resp.json())
```

The plugin forwards the body verbatim and relays the upstream body back,
preserving status code and `Content-Type` (+ `X-Msh-Request-Id` when present).

## unified-finance-mcp wiring

Point `unified-finance-mcp` at the plugin endpoint and give it the management
key. The kimi provider treats this as a proxy hop: it does not attach its
own bearer, and it sends the `X-Msh-*` headers as usual.

```dotenv
KIMI_PROXY_URL=http://localhost:8317/v0/management/kimi/tools
KIMI_ACCESS_TOKEN=<MANAGEMENT_KEY>
```

(Setting `KIMI_ACCESS_TOKEN` to the management key is what makes the client
attach `Authorization: Bearer …`; the plugin strips this header before
talking to Kimi, so the token value never leaves the cliproxy boundary.)

## Registering with the Plugin Store

This repository is pre-configured for the [CLIProxyAPI-Plugins-Store](https://github.com/router-for-me/CLIProxyAPI-Plugins-Store). The release workflow produces `kimi-tools_<ver>_<os>_<arch>.zip` assets whose shape matches the store's `install.type: github-release` installer.

To publish:

1. Confirm this repository is public and tagged `v0.1.0`.
2. Wait for `.github/workflows/release.yml` to upload the six platform zips.
3. Open a PR against `router-for-me/CLIProxyAPI-Plugins-Store` adding the entry below to `registry.json` (between the other `Provider` / `Management` entries):

```json
{
  "id": "kimi-tools",
  "name": "Kimi Tools",
  "description": "Forwards Management API calls to Kimi Datasource (api.kimi.com/coding/v1/tools), reading access_token/device_id per request from a cliproxy kimi auth JSON.",
  "author": "xyonium",
  "version": "0.1.0",
  "repository": "https://github.com/xyonium/cliproxy-kimi-tools",
  "homepage": "https://github.com/xyonium/cliproxy-kimi-tools",
  "license": "MIT",
  "tags": ["Provider", "Management"]
}
```

The store's installer expects the release zip to contain exactly one platform artifact named `kimi-tools.so` / `kimi-tools.dylib` / `kimi-tools.dll` at the zip root; the workflow emits that shape.

## Response sanitization

CLIProxyAPI's `ServeManagementHTTP` may apply `htmlsanitize` to JSON-looking
bodies. Kimi datasource responses are JSON (`is_success` / `result` /
`error`), so this is safe — no binary payloads traverse this route.

## Limitations

* Route path is `/v0/management/kimi/tools`, not `/v1/tools` or
  `/coding/v1/tools` — CLIProxyAPI reserves the `/v0/management` prefix for
  plugin-registered management routes.
* Requests require the CLIProxyAPI management key (already required for any
  `/v0/management/...` route).
* This is a **pure plugin** — no CLIProxyAPI modifications required, but
  also no special access to the AuthManager; the plugin re-reads the
  configured auth file rather than sharing the keeper's credential pool.

## License

MIT
