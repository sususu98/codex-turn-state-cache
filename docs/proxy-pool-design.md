# Plugin-only prewarming: simple YAML and optional account selection

## Common configuration (v0.5.0)

Inside `plugins.configs.codex-turn-state-cache`:

```yaml
prewarm:
  enabled: true
  models: [gpt-6-astra]
  max_probes_per_hour: 384
  proxies:
    - "socks5h://user:password@proxy.example:1080"
  # Optional; omit to discover every eligible enabled Codex account.
  accounts:
    - email: someone@example.com
    - auth_id: codex-example.json
```

The four normal fields are `enabled`, `models`, `max_probes_per_hour`, `proxies`.
`accounts` is an optional filter, not a requirement. Email matching is exact
(case-insensitive); AuthID matching is exact (case-sensitive). Each selector uses
exactly one field. Multiple selectors are ORed and duplicate matches are warmed
once. Multiple accounts sharing an email are all eligible matches, independently
scoped by AuthID. Selectors cannot enable a disabled account.

Proxy entries are SOCKS5H URLs. Four-field `host:port:username:password` entries
remain accepted for migration; URL syntax is required for IPv6. Credentials are
URL-escaped and duplicates removed. Inline entries and legacy `proxy_file` cannot
both be supplied. Invalid lists fail reload before replacing the running plugin.

**Credential visibility:** proxy passwords now live inside CPA YAML, readable by
users authorized to read that configuration (including management config/YAML
endpoints). Metadata hiding does not remove this visibility. Use mode 0600, keep
backups private and never commit the live YAML. Plugin errors/logs do not echo
proxy credentials or raw YAML parse errors.

## Account discovery

- Existing read-only `host.auth.list/get_runtime/get` callbacks only; no Host
  source changes, new API, auth writes, refresh or account enable/disable calls.
- Discovery starts after a five-second warmup and refreshes every 60 seconds.
  An unavailable directory is retried after 10 seconds, not permanently cached
  as an empty set. Discovery itself makes no paid upstream calls.
- Only enabled, file-backed Codex OAuth accounts with a known supported plan,
  usable identity and native Codex upstream are eligible. Unknown plans,
  runtime-only accounts and custom upstreams are not probed.
- Plan comes from account metadata or saved JWT hints; this is not independent
  JWT verification. Pro/Plus/Free/Individual use the existing 292-byte family;
  Team/Business/self_serve_business_prolite use 332. No plan is inferred from the
  state length. Disabled/unknown accounts keep selector identity but no jobs.
- Account indices are replaced on refresh. Added/re-enabled matches become
  eligible on a subsequent refresh; removed/disabled/plan-changed accounts lose
  their usable ticket. Every actual probe/Bind independently rechecks status.
- Automatic mode remains verified-only for configured models while discovery is
  unavailable/empty. With filters, unselected accounts retain legacy passive
  behavior after initial discovery. Previously selected identities stay guarded
  against passive fallback after removal/plan loss. Retained ownership is bounded;
  extreme identity churn conservatively guards the configured models instead.
- Generation checks reject old queued/in-flight work after removal and re-add.

## Internal defaults and compatibility

| Setting | Default |
| --- | --- |
| Workers | 2 |
| Attempts per account/model round | 6 |
| Delay between attempts | 2 seconds |
| Failure cooldown | 300 seconds |
| Probe timeout | 20 seconds |
| Ticket TTL | 3600 seconds from acquisition |
| Refresh before expiry | 300 seconds |
| Pending observations | 20000 |
| Hourly budget when omitted | 12 requests |

The example explicitly chooses 384; it is not a hardcoded budget. All old
advanced limit fields remain optional overrides. Both acquisition and formal
verification count, including failed requests. The native-runtime budget survives
ordinary reconfiguration/quiesce, but process/native-image replacement resets it.
This is a request-count cap, not a monetary cap.

Legacy explicit account objects (`auth_id`, `plan`, per-account `models`) and
`proxy_file` remain compatible. Do not mix those full objects with new selectors.
New selector mode uses top-level `models`; no repeated plan/model blocks needed.

The Host config file is discovered from `-config PATH`, `--config PATH`, or their
`=PATH` forms; otherwise ordinary file mode uses working-directory `config.yaml`.
The old `host_config_file` override remains supported. Home/PostgreSQL/ObjectStore/
GitStore modes are refused rather than guessing the effective formal route.
The source guard rereads the live native process environment because Host `.env`
loading can occur after plugin registration. Unsupported ambiguous startup flags
also fail closed. No Host configuration is written by the plugin.

## Transport and qualification

Acquisition uses only the background pool. The formal exit is account JSON
`proxy_url`, then the Host file's global proxy, then explicit direct mode.
Malformed/unreadable/changing configuration or invalid proxies never silently
fall back to direct/environment proxies. The plugin's Chrome uTLS/HTTP2 client
owns one connection per probe; close/cancellation releases it. Tokens stay in
plugin memory and go only to the fixed HTTPS native Codex endpoint. Redirects,
custom upstreams and unbounded response reads are refused.

Probes use a fixed tool-free `ping → pong`, a context timeout for network I/O and
4 MiB body limit. **This is not a hard wall-clock bound on the entire operation:**
existing native `host.auth.*` callbacks are synchronous and cannot be interrupted
by the plugin's context. The 30-second discovery and 20-second probe contexts are
checked around these calls, but a blocked callback (or synchronous local file I/O)
can overrun them. Shutdown joins workers/drains callbacks and can consequently
wait indefinitely if a Host callback never returns. Do not fake cancellation by
abandoning the callback in a goroutine and unloading the native image. Guaranteed
cross-boundary cancellation requires a separate Host capability or process
isolation design, neither of which is implemented or authorized here.
Acquisition requires HTTP 200, an explicit successful completion with the exact
model, the plan's encoded length, `gAAAAA` prefix and token-safe characters.
The candidate is then injected on the same account's unchanged formal exit.
That verification must complete with the same model and no 312-byte/conflicting
state header; another 292/332 response header is not required. The original
candidate is cached. A successful acquisition proxy may be preferred by other
background workers, never by formal business traffic.

Proxy IDs in logs are pool slots, not addresses/passwords. A selected proxy shown
with `budget_exhausted` may not have been dialed. A round's final failure log is
not a count or exhaustive record of every underlying probe. Do not infer that
all preceding proxies or every account/proxy combination was tested from it.

Cached state is scoped to account/model plus identity, known plan, formal route,
relevant headers and TTL. Reads do not renew it; failed renewal keeps a usable
old ticket. Reads/resolution never use refresh_token or change CPA account status.
Normal OAuth refresh remains CPA's responsibility. Missing tickets do not block
business requests; caller-owned state is preserved. Probe usage bypasses normal
CPA inference usage reporting and still incurs upstream fees.

A visible completed-model mismatch, incompatible known plan, 312/conflicting
state header revokes only the injected ticket version and schedules recollection.
Incomplete business streams alone are not downgrade evidence. Some translated
Host callbacks hide raw upstream completion models; watchdog coverage is therefore
not universal. Rebound active RequestIDs suppress ambiguous watchdog callbacks
because the ABI lacks attempt identity. WS proxy routing/rotation is out of scope.

No token decryption, model-quality benchmark or proof of universal cross-IP
acceptance is claimed. Model response labels are checks, not proof of backend
hardware identity. Unsaved private runtime proxy changes cannot be inferred from
file-backed configuration; do not enable this mode for such accounts.

## Verification and rollout

Run `go test -race -count=1 ./...`, `go vet ./...`, native ABI smoke and library
builds. Tests cover minimal/legacy config, email/AuthID selection, disabled/unknown
accounts, dynamic removal/re-add, discovery races, path inference, secret-safe
errors and loopback SOCKS domain addressing/auth/cancellation.

Deploy only the plugin and its configuration. Compare the original Host binary
SHA-256 and checkout status before/after; never rebuild/patch Host. Migration must
preserve current model/account scope, budget and all non-plugin settings, with
private backups. Real upstream validation needs an explicitly enabled budgeted
run; mocks do not prove live success.
