# Codex Turn-State Cache

Native CLIProxyAPI plugin that reuses a qualifying upstream
`X-Codex-Turn-State` header on later Codex requests.

## Behavior

- Accepts exactly one upstream header value. With `X-Codex-Plan-Type`,
  `pro`/`plus`/`free`/`individual` require 292 raw bytes;
  `team`/`business`/`self_serve_business_prolite` require 332 raw bytes.
  Missing or unknown plans use `accepted_lengths` (default `[292, 332]`).
  These are encoded header lengths, not Base64-decoded lengths.
- Keys state by CPA's selected account and the exact after-auth model.
- Reuses a matching account/model state across client or egress IP changes.
- Replaces a prior valid value for the same key and gives the new value a fixed
  one-hour lifetime. Reads do not extend that lifetime.
- Captures HTTP responses, stream header initialization, and upstream
  WebSocket response events (such as `codex.response.metadata` and
  `codex.rate_limits`). A completed request cannot later populate the cache.
- Keeps state only in process memory. Reconfiguration, quiesce, unload, and
  CPA restart clear it.

Successful captures and injections use CPA's native `host.log` callback. Logs
include the plugin ID, model, and one of the following messages, but never a
state value, account ID, IP, request header, or body:

```text
codex turn-state cache captured source=http
codex turn-state cache captured source=stream plan=pro
codex turn-state cache captured source=websocket plan=self_serve_business_prolite
codex turn-state cache captured source=<http|stream|websocket> replaced=true
codex turn-state cache injected
```

## CPA Configuration

Install from the CPA plugin store, or place the Linux/amd64 library at:

```text
/CLIProxyAPI/plugins/linux/amd64/codex-turn-state-cache-v0.3.0.so
```

Then enable it in CPA configuration:

```yaml
plugins:
  enabled: true
  dir: /CLIProxyAPI/plugins
  configs:
    codex-turn-state-cache:
      enabled: true
      priority: 0
      max_entries: 10000
      max_pending_entries: 20000
```

`max_entries`, `max_pending_entries`, and `accepted_lengths` are optional.
Known plan/length pairs remain strict regardless of `accepted_lengths`.
The one-hour lifetime is fixed.

## Build

Build the Linux/amd64 shared library locally or in CI, not on the CPA host.

```powershell
$zig = 'C:\path\to\zig.exe'
$env:CGO_ENABLED = '1'
$env:CC = "$zig cc"
go test ./...
go vet ./...

$env:GOOS = 'linux'
$env:GOARCH = 'amd64'
$env:CC = "$zig cc -target x86_64-linux-gnu.2.17"
go build -trimpath -buildmode=c-shared `
  -o dist/codex-turn-state-cache-v0.3.0.so ./cmd/plugin
```

## Release Asset

The GitHub Release supports Linux/amd64 only. Its zip asset is named
`codex-turn-state-cache_0.3.0_linux_amd64.zip`, contains only
`codex-turn-state-cache.so` at the archive root, and is verified by the
adjacent `checksums.txt` file.

## Related Links

- [LINUX DO](https://linux.do/) — 新的理想型社区
