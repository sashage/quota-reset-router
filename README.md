# quota-reset-router

A [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) scheduler plugin for Claude and Codex OAuth accounts. This fork **balances requests using remaining quota and reset urgency**, favoring soon-to-expire quota while preserving multiple usable subscriptions for bursts and sustained demand.

Fork: [sashage/quota-reset-router](https://github.com/sashage/quota-reset-router). Based on [WebDevCaptain/quota-reset-router](https://github.com/WebDevCaptain/quota-reset-router); the upstream `weekly_reset_first` policy remains available.

- **Author:** Shreyash ([webdevcaptain](https://github.com/webdevcaptain))
- **License:** [MIT](LICENSE)

> Not affiliated with or endorsed by Anthropic, OpenAI, or CLIProxyAPI. Check each provider's terms before routing subscription credentials through a proxy.

## Selection rules

- Claude and Codex pools are ranked independently. Requests spanning multiple providers are left to CLIProxyAPI.
- **Default policy:** `quota_balanced` uses smooth weighted rotation on every request, including between quota polls. Accounts with more remaining quota and sooner weekly resets receive larger shares.
- **Soft protection:** outside the final 24 hours, low weekly balances lose share more strongly. This protection eases continuously inside the final day. There is no hard reserve: one remaining usable account can serve all demand.
- **Short/model windows:** low five-hour headroom reduces share; relevant Sonnet/Opus headroom also constrains the weekly balance used for scoring.
- **Skip:** accounts whose five-hour, weekly, or model-specific (Sonnet/Opus) quota is exhausted.
- Existing credential `priority` tiers still take precedence. Use equal account priorities to balance all subscriptions together.
- Only candidates offered by CLIProxyAPI are considered, so its model eligibility, cooldowns, and retries still apply.
- Codex: the earliest reset among its weekly or monthly windows is used for ranking.

Example shares, with equal five-hour headroom and no model-specific constraint:

| A remaining / resets in | B remaining / resets in | Approximate request share A / B |
|---|---|---|
| 80% / 2 days | 80% / 5 days | 80% / 20% |
| 10% / 2 days | 80% / 5 days | 6% / 94% |
| 80% / 6 hours | 100% / 7 days | 99% / 1% |
| 2% / 3 days | 2% / 3 days | 50% / 50% |

The heuristic uses `weight = remaining^exponent / hours^1.5 * short_remaining`, where remaining values are fractions, `hours = max(1, hours_until_weekly_reset)`, and `exponent = 1 + min(1, hours / 24)`. Relevant model-specific remaining quota caps `remaining`. Missing short-window data uses a factor of 1. Weights are normalized within the eligible highest-priority provider pool, then smooth weighted rotation distributes individual requests. Equal credits break ties by credential ID. Reconfiguration restarts rotation; shadow mode simulates the same rotation without routing.

This is a request-share heuristic, not an optimal quota-spend guarantee: request costs differ, quota observations lag, and other clients may consume the same accounts. It assumes equal plan capacities per provider; percentages do not reveal absolute capacity on mixed plans. Short-window reset times enforce eligibility and trigger refreshes, but do not increase urgency weights. Use `selection_policy: weekly_reset_first` for the original strictly earliest-weekly-reset behavior.

## Quota polling

- One background worker. Request routing uses cached data and makes no network calls.
- Refreshes every `poll_interval` (default 5 minutes) and shortly after any reported reset. Checks for credential changes every 30 seconds.
- Read-only `GET` requests to:
  - `https://api.anthropic.com/api/oauth/usage`
  - `https://chatgpt.com/backend-api/wham/usage`
- No model requests, token refreshes, or credential writes.

## Fallback

The plugin hands the request back to CLIProxyAPI's configured routing strategy when it has no trustworthy data:

- before the first successful quota refresh
- after a 401 or 403 from a quota endpoint
- when cached quota is older than `max_age` (other refresh errors keep the last good data until then)
- when no eligible account has known quota

Ordering is not guaranteed in these cases. Quota can also change between refreshes.

## Requirements

- CLIProxyAPI v7.3.15 (plugin ABI 1, schema 6). Other versions are untested.
- Linux amd64 or arm64 (glibc 2.34 or newer), macOS 12 or newer on Apple silicon or Intel, or Windows amd64.
- The Intel macOS build uses a patched Go toolchain. See [Intel Macs](#intel-macs).
- Direct network access to both quota endpoints. Credentials with `proxy_url` or `base_url` are skipped. Quota polling ignores CLIProxyAPI's `proxy-url` and proxy environment variables.

## Install

1. Build this fork using the development instructions below, or obtain a release built from this fork. Upstream release binaries do not contain `quota_balanced`. For fork release archives, check `checksums.txt` and verify build provenance with `gh attestation verify <zip> --repo sashage/quota-reset-router`.
2. Extract `quota-reset-router.so` (Linux), `quota-reset-router.dylib` (macOS), or `quota-reset-router.dll` (Windows) into CLIProxyAPI's plugin directory (`plugins.dir`) as a regular file. Symlinks are not loaded.
3. Merge into `config.yaml`:

   ```yaml
   plugins:
     enabled: true
     dir: plugins
     configs:
       quota-reset-router:
         enabled: true
         priority: 10
         mode: shadow
         selection_policy: quota_balanced
   ```

4. Restart CLIProxyAPI.
5. Review the status endpoint. When the proposed choices are correct, switch `mode` to `active`.

`priority` orders plugins, not accounts. CLIProxyAPI consults only the highest-priority scheduler plugin.

This fork retains the `quota-reset-router` plugin ID. An update from the upstream plugin-store entry can replace the fork with upstream behavior; use fork-built artifacts when upgrading.

## Configuration

| Key | Default | Allowed | Purpose |
|---|---|---|---|
| `mode` | `shadow` | `shadow`, `active` | `shadow` records proposed choices only. `active` routes requests. |
| `selection_policy` | `quota_balanced` | `quota_balanced`, `weekly_reset_first` | Balance headroom and reset urgency, or use the upstream policy. |
| `poll_interval` | `5m` | `1m` to `1h` | Quota refresh interval. |
| `max_age` | `10m` | `poll_interval` to `1h` | Maximum age of cached quota. |
| `request_timeout` | `10s` | `1s` to `30s` | Timeout per quota request. |

Change settings without a restart:

```text
PATCH /v0/management/plugins/quota-reset-router/config
{"mode": "active"}
```

## Status

```text
GET /v0/management/plugins/quota-reset-router/status
```

Requires Management API authentication. Returns the version, mode, selection policy, per-account quota snapshots, refresh errors, the last decision, and routing counters. In balanced mode, `last_decision.traffic_shares` records the normalized target shares for that decision's eligible accounts (fractions, not observed usage). Credential IDs are included (often file names containing email addresses). OAuth tokens are not.

## Disable or upgrade

- **Disable** without a restart. CLIProxyAPI's configured routing resumes after it reloads the configuration:

  ```text
  PATCH /v0/management/plugins/quota-reset-router/enabled
  {"enabled": false}
  ```

- **Upgrade:** stop CLIProxyAPI, replace the plugin file, start CLIProxyAPI.

## Limitations

- Native plugins run inside the CLIProxyAPI process with access to its credentials and traffic. Expected errors fall back to CLIProxyAPI routing; a native crash can still affect the proxy.
- When the plugin selects an account, CLIProxyAPI's built-in strategy, including session affinity, is not used for that request.
- CLIProxyAPIHome dispatch does not consult plugin schedulers.
- The quota endpoints are not stable public APIs. Unrecognized responses trigger fallback.

## Development

Build and test targets take `TARGET`: `linux_amd64` (default), `linux_arm64`, `darwin_amd64`, `darwin_arm64`, or `windows_amd64`.

- Linux and Windows builds, `linux-test`, `native-test`, and Linux `host-test` run in a pinned Docker image.
- macOS builds need a macOS host with Go 1.21+ and the Xcode Command Line Tools. Go 1.26.8 is downloaded automatically.
- `darwin_amd64` first builds the patched toolchain into `dist/_go-tls` (about a minute, once per checkout), then uses it for `make test` and `make build`.
- `make test` needs Go 1.26+ and a C toolchain.

```sh
make test         # gofmt, go vet, and unit tests with the race detector
make linux-test   # same, in the pinned Linux container
make build        # writes dist/<target>/quota-reset-router.<so|dylib|dll>
make native-test  # Linux only: loads the library through the plugin C ABI
make host-test    # Linux, or macOS CI: runs the official CLIProxyAPI release with the library
make zip          # writes dist/release/quota-reset-router_<version>_<target>.zip
make load-test    # loads the zip into the official CLIProxyAPI; TARGET must match this machine
```

- `host-test` and `load-test` download the official CLIProxyAPI v7.3.15 release and verify its checksum.
- `native-test` and `host-test` run with networking disabled, local TLS fixtures, and synthetic credentials. On macOS, `host-test` instead needs the quota hosts pinned to 127.0.0.1 and the fixture certificate trusted in the System keychain, which CI sets up; `SOAK_SECONDS` adds a soak under GC pressure.
- `load-test` starts CLIProxyAPI without credentials, so the plugin makes no network requests.
- CI builds every target and loads each zip into the official CLIProxyAPI on its own platform.
- `make clean` removes build output.

### Intel Macs

Stock Go on macOS amd64 keeps each thread's current goroutine in one fixed thread-local slot, slot 6, which Apple reserves for Go. Every Go runtime in a process uses that slot. CLIProxyAPI is itself a Go program, so a plugin built with stock Go runs on CLIProxyAPI's goroutines and heap, and CLIProxyAPI v7.3.15 crashes at startup (`fatal error: addspecial on invalid pointer`; CLIProxyAPI's own Go scheduler example fails with `unknown caller pc`). Linux, Windows, and Apple silicon give each runtime its own slot, so they are unaffected.

The `darwin_amd64` library is built with Go 1.26.8 plus [`scripts/go-tls-slot.patch`](scripts/go-tls-slot.patch). The patch changes two constants so the plugin's runtime uses slot 11, which Apple also reserves and leaves unused. CLIProxyAPI keeps slot 6. The toolchain is built from the checksum-verified upstream source, and the build fails if any goroutine access in the library still uses slot 6.

## Release

The CLIProxyAPI plugin store installs from this repository's latest published GitHub release. Each release contains `checksums.txt` and one `quota-reset-router_<version>_<os>_<arch>.zip` per target, holding the plugin, `LICENSE`, and `THIRD_PARTY_NOTICES.md`.

1. Set `pluginVersion` in `config.go`, for example `0.2.0`, and merge to `main`.
2. Push the tag `v<version>`. CI builds and tests every target, attests build provenance, and creates a draft release with the assets.
3. Review the draft and publish it.

`make release-assets` builds the same set locally on a macOS host with Docker.

## Third-party notices

Binary releases link the CLIProxyAPI plugin SDK (MIT), `gopkg.in/yaml.v3` (MIT and Apache-2.0), and the Go standard library (BSD-3-Clause; patched on `darwin_amd64`). Windows builds also statically link parts of the MinGW-w64 runtime. See [THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
