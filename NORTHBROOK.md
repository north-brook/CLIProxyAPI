# Northbrook quota-aware routing

This fork adds a standalone Claude OAuth routing strategy on top of upstream
CLIProxyAPI v8.0.43. Codex WebSockets and the other existing transports are preserved.

```yaml
routing:
  strategy: quota-aware
  session-affinity: true
  session-affinity-ttl: 24h
  session-affinity-subagents: true
  quota-aware:
    refresh-interval: 5m
    max-age: 15m
    reserves:
      - provider: claude
        account: personal@example.com
        percent: 20
```

Account matching uses the OAuth credential's email, case insensitively. No
credentials or personal account configuration belong in this repository.

## Selection

For each eligible Claude OAuth account, each applicable quota window contributes:

```
spend rate = (remaining percentage - reserved percentage) / seconds until reset
account score = minimum spend rate across its applicable windows
```

The highest score wins a new binding. This favors allowance that can be spent
faster before it expires, while respecting the most restrictive quota window.
Five-hour and shared weekly windows are required for a protected account;
reported Opus/Sonnet weekly windows also apply when routing that model family.
An unused zero-utilization window without a reset is treated as a full-duration
window until the provider reports its actual first-request reset.

Fixed account priorities only break equal scores or provide a fallback when no
unprotected account has fresh, complete quota data. Other providers and Claude
API keys retain priority-based round-robin behavior. A healthy session keeps its
existing account even when another account's score rises.

## Reserves and freshness

A request cannot select an account at or below its reserve in any applicable
window. This check precedes explicit and inferred session affinity, subagent
inheritance, pinned account selection, and retries. When necessary, a session
moves to another eligible account. If none is eligible, the proxy returns a
`quota_reserve` error rather than consuming reserved allowance.

Protected accounts with missing, invalid, expired, or stale quota data are excluded.
Unprotected accounts with unknown quota remain available as a fallback. Existing
provider cooldowns, model availability, and disabled-account rules still apply.
Reserves require the quota-aware strategy; invalid configuration is rejected on
startup and reload. Home dispatch is not supported because it owns account
selection outside this process.

Quota headers from ordinary Claude responses update the existing passive quota
snapshot. A background task also reads the authenticated Claude usage endpoint
every five minutes by default, covering accounts used outside the proxy. HTTP 429
probes back off for at least fifteen minutes and honor a longer Retry-After.
There are no usage probes on the inference request path. Probe results cannot
overwrite a newer response snapshot or a replaced credential.

The reserve is an admission threshold based on provider observations, not a
guarantee of an exact remaining balance: already-running requests, delayed
provider accounting, and simultaneous Claude app usage can cross it. Set a larger
reserve if additional margin is needed. The proxy does not cancel running work.

Configuration changes use the existing file watcher or v8 management config API.
No management UI changes are required. The existing auth-file quota observations
expose the signals used by the router.

## Build and verify

Use Go 1.26.10 or newer and the repository's normal recipes:

```sh
go test ./...
go test -race ./sdk/cliproxy/auth ./sdk/cliproxy -run 'TestQuota|TestClaudeUsageHeaders'
go build -o cli-proxy-api ./cmd/server
```

On macOS, point DEVELOPER_DIR and SDKROOT at a matching installed Xcode toolchain
if the default compiler cannot read the selected SDK's text-based libraries.
