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
- Passive mode does not include client/egress IP in its key. This is not a
  guarantee of cross-exit acceptance; optional prewarming verifies on the formal route.
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
/CLIProxyAPI/plugins/linux/amd64/codex-turn-state-cache-v0.5.0.so
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
  -o dist/codex-turn-state-cache-v0.5.0.so ./cmd/plugin
```

## Release Assets

CI builds Linux and macOS for amd64 and arm64 (aarch64). Version-tag builds
publish shared libraries, ZIP archives and `checksums.txt`; ordinary pushes
publish workflow artifacts. Building does not deploy or enable prewarming.

## Optional verified prewarming (v0.5.0, plugin-only)

Normal configuration lives in the same YAML; no proxy TXT file or repeated plan
settings are needed. Inside `plugins.configs.codex-turn-state-cache`:

```yaml
prewarm:
  enabled: true
  models: [gpt-6-astra]
  max_probes_per_hour: 384
  proxies:
    - "socks5h://user:password@proxy.example:1080"
  # Optional: warm only these accounts. Omit to discover all eligible accounts.
  accounts:
    - email: someone@example.com
    - auth_id: codex-example.json
```

Email matching is exact and case-insensitive; AuthID matching is exact. Each
selector specifies one or the other. If several accounts share an email, all
eligible matches are selected. Disabled accounts are always excluded. Discovery
refreshes every minute; unknown plans are not guessed to be Pro.

A probe-only SOCKS5H pool fills an independent verified cache for the selected
account/model pairs. Business requests keep their normal exit.

1. Acquire through the pool: HTTP 200, successful completion, exact returned
   model, `gAAAAA` token-safe state and configured Pro 292 / Team 332 bytes.
2. Inject on the same account's **current formal route**, complete another
   request with the exact model, and reject 312-byte/conflicting state headers. Like the
   reference design, verification does not require another 292/332 response
   header. Cache the original candidate, not the validation response.
3. Reuse only while account identity, formal/config scope and TTL match.
   Visible completed-model mismatches, 312-byte/conflicting headers or known
   plan mismatches revoke the matching
   ticket version and schedule recollection. Failed renewal keeps a usable ticket.

No token decryption, model quality benchmark or proof of IP independence is
claimed. Plan is obtained from account metadata/saved JWT hints, not inferred
from state length. Unknown plans do not start automatic probes. Legacy explicit
account/plan configuration remains supported. Passive captures never overwrite
verified tickets.

**No Host changes or new Host API.** The plugin uses existing read-only
`host.auth.list/get_runtime/get` callbacks, discovers the ordinary Host config
path from its `-config` arguments (or working-directory `config.yaml`), and uses
its own SOCKS5H/uTLS/HTTP2 client. The config is read-only. No special storage or
Home-managed backend is guessed. Legacy `host_config_file` and `proxy_file`
overrides remain compatible.

Concurrency, retries, cooldown and TTL have internal defaults; the hourly budget
remains configurable and includes acquisition plus formal verification. Account
tokens are handled in plugin memory, never logged, refreshed or saved. Inline
proxy passwords are visible to authorized configuration readers: protect YAML
permissions and keep it out of Git. See [details](docs/proxy-pool-design.md).

## Related Links

- [LINUX DO](https://linux.do/) — 新的理想型社区
