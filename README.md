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
