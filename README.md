# synthetic-monitor

A synthetic monitoring service in Go. It probes HTTP(S), TCP, DNS and TLS
endpoints on a schedule, records availability and latency, tracks SLOs and
error budgets, and sends a webhook alert when a check breaks and a notice when
it recovers.

Status: v1 in progress.

## Development

Requires Go 1.27.3 or later.

```sh
go run ./cmd/synthetic-monitor        # serves http://localhost:8080/healthz
go test -race ./...
golangci-lint run
```

## Decisions

Each entry records what was chosen, the alternatives, and why.

### Standard library first

The HTTP server uses `net/http` without a framework: since Go 1.22 `ServeMux`
routes on method and path wildcards, which covers everything this service
needs. Logging uses `log/slog`. A third-party module is added only when the
alternative is reimplementing a well-solved problem, and each one is listed
here with its reason.

### Goroutine leak checks in every test package

`go.uber.org/goleak` (test-only) runs after each package's tests and fails
them if any goroutine they started is still alive. Go 1.27's built-in
`goroutineleak` profile was the stdlib alternative, but it only reports
goroutines blocked forever on a channel or lock that nothing can reach. It
would miss a scheduler loop that keeps ticking after `Stop()`, which is the
leak this service is most likely to have.

### CI actions pinned to commit SHAs

A Git tag such as `v7` can be moved to point at different code, so a
compromised action repository can change what runs in CI without any change
to this repository. Pinning to a full commit SHA makes the workflow run exactly
the reviewed code. Each pin carries a comment with its version for readability.

### Config: strict YAML, one block per check type

The checks file is decoded with `go.yaml.in/yaml/v3`, the YAML organisation's
maintained continuation of `gopkg.in/yaml.v3` (whose author marked it
unmaintained in April 2025). `sigs.k8s.io/yaml` was the alternative: it
converts YAML to JSON and reuses `encoding/json`, but it is a wrapper over the
same parser and adds a conversion step.

Unknown fields are rejected. A typo such as `timout: 5s` would otherwise be
silently ignored and the check would run with the default timeout, which is
the kind of bug that only shows up during an incident. Durations must carry a
unit (`30s`, not `30`), and every validation problem is reported at once.

A check's type comes from which block it sets (`http:`, `tcp:`, `dns:` or
`tls:`) instead of a separate `type:` field, so a type and its settings can
never disagree. Shared settings use YAML anchors rather than a custom
`defaults:` section.

The check `name` is its identity: config reloads, metrics, history and alert
state are all keyed by it.

### HTTP probes open a fresh connection every time

Keep-alives are disabled, so every probe resolves DNS, connects and completes
a TLS handshake, as a new visitor would. With connection reuse, a broken DNS
record or an expired certificate could go unnoticed for as long as an old
connection stayed open, and latency would look better than real users see it.
The cost is a little more work per probe, which is negligible at monitoring
rates. `httptrace` splits each probe's time into DNS, connect, TLS, server
processing and transfer, so a slow probe shows where the time went.

### One wrapper owns timeouts

Each probe type only knows how to do its own work and classify its own
errors. A shared wrapper applies the check's timeout through the context,
measures duration and enforces `max_latency`. When the deadline expires, a
probe can fail with many different errors (an i/o timeout, a cancelled read,
a DNS timeout); the wrapper reports all of them as `timeout`, so the failure
reason does not depend on which step was running at the time. A probe
cancelled by its caller, for example at shutdown, is reported as `canceled`
and is not the target's fault.

### One resolver for every probe, released as soon as a probe gives up

All probes resolve names with Go's own DNS client rather than the platform's
(`PreferGo`). It behaves the same on macOS and in the Linux container, has no
cache, so every probe sends real queries, and honours the probe's context.

The goroutine leak tests found one gap: when a probe times out, Go's resolver
cancels the lookup, but the UDP read in flight is bounded only by its
per-attempt deadline from `resolv.conf` (5 seconds by default). Each timed-out
DNS probe left a goroutine and a socket behind for up to 5 seconds, which
piles up against a dead nameserver. The resolver's dial function now closes
the connection as soon as the lookup's context ends.
