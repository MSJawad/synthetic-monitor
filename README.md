# synthetic-monitor

A synthetic monitoring service in Go. It probes HTTP(S), TCP, DNS and TLS
endpoints on a schedule, records availability and latency, tracks SLOs and
error budgets, and sends a webhook alert when a check breaks and a notice when
it recovers.

Status: v1 in progress.

## Running

```sh
go run ./cmd/synthetic-monitor -config examples/checks.yaml -log-level debug
```

| Flag | Default | Purpose |
|---|---|---|
| `-config` | (required) | checks file, see [`examples/checks.yaml`](examples/checks.yaml) |
| `-listen` | `:8080` | HTTP server address |
| `-history` | `history.db` | SQLite file of probe results |
| `-history-retention-days` | `35` | days of results kept; must cover every SLO window |
| `-max-concurrent-probes` | `64` | cap on probes in flight across all checks |
| `-config-poll-interval` | `5s` | how often the checks file is checked for changes |
| `-shutdown-timeout` | `30s` | how long probes in flight get to finish after SIGTERM |
| `-log-level` | `info` | `debug` also logs every successful probe |

Edits to the checks file are applied within one poll interval, or at once on
`SIGHUP`. An invalid file is rejected and logged, and the previous checks keep
running. On `SIGTERM` or Ctrl-C, `/readyz` turns unready, no new probes start,
and probes in flight finish before the process exits.

| Endpoint | Purpose |
|---|---|
| `GET /metrics` | Prometheus metrics |
| `GET /healthz` | liveness: the process is up |
| `GET /readyz` | readiness: `503` once shutdown has started |

### Metrics

Per-check series are labelled only with `check`; join on
`synthetic_check_info` for a check's `type`.

| Metric | Type | Meaning |
|---|---|---|
| `synthetic_probes_total{check,result}` | counter | probes completed; `result` is `success` or the failure reason |
| `synthetic_probe_success{check}` | gauge | 1 if the last probe succeeded |
| `synthetic_probe_duration_seconds{check}` | histogram | probe duration |
| `synthetic_probe_http_phase_duration_seconds{check,phase}` | histogram | HTTP time by phase: `dns`, `connect`, `tls`, `processing`, `transfer` |
| `synthetic_tls_cert_expiry_timestamp_seconds{check}` | gauge | when the certificate chain stops verifying |
| `synthetic_check_info{check,type}` | gauge | 1 per configured check |
| `synthetic_slo_target_ratio{check}` | gauge | SLO target, e.g. `0.995` |
| `synthetic_slo_availability_ratio{check}` | gauge | fraction of probes in the SLO window that succeeded |
| `synthetic_slo_error_budget_remaining_ratio{check}` | gauge | error budget left; negative once overspent |
| `synthetic_scheduler_lag_seconds` | histogram | how late probes started, mostly waiting for a free slot |
| `synthetic_scheduler_skipped_runs_total{check}` | counter | runs skipped because an earlier probe overran |
| `synthetic_probes_in_flight` | gauge | probes running now |
| `synthetic_config_reloads_total{result}` | counter | config loads by `success` or `failure` |
| `synthetic_config_last_reload_successful` | gauge | 0 while a rejected config is being ignored |
| `synthetic_history_dropped_results_total` | counter | results not stored because the write queue was full |
| `synthetic_history_write_errors_total` | counter | failed history writes |

Failure reasons: `timeout`, `dns`, `connect`, `tls`, `cert_expiry`,
`http_status`, `http_body`, `dns_answer`, `latency`, `other`.

## Development

Requires Go 1.27.3 or later.

```sh
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

### TLS checks fail before a certificate expires, not after

A TLS check performs a full verified handshake, so an expired, untrusted or
wrong-host certificate fails it as `tls`. A valid chain that expires within
`min_days_remaining` (default 14) fails it as `cert_expiry`. That gives an
alert while there is still time to act: automated renewal normally happens
around 30 days before expiry, so a certificate with under 14 days left means
renewal is broken.

The expiry date is the earliest `NotAfter` of any certificate in the chain,
not just the leaf, because an expired intermediate breaks verification just
as an expired leaf does. If the server's certificate verifies through several
chains (for example via a cross-signed intermediate), the connection keeps
working while any of them is valid, so the check uses the chain that lasts
longest.

### Scheduler: one goroutine per check, a shared limit on probes in flight

Each check has its own goroutine with its own timer, and a semaphore caps the
probes running at once across all checks. The alternative was a single timer
heap feeding a fixed pool of workers, which scales to hundreds of thousands
of checks with a constant number of goroutines. But it puts every check's
schedule in one shared structure that reloads must edit under a lock. With
one goroutine per check, stopping a check is cancelling its context, and a
goroutine costs a few kilobytes, so even ten thousand checks are cheap. The
benchmarks measure where this design stops being enough.

Each check's first probe comes at a random point within its interval, so
checks that share an interval do not all fire together after a start or
reload. The spread is capped at one minute: spreading an hourly check's
first probe over a whole hour would leave it without data for up to an hour
after every deploy. After that it keeps a fixed cadence: the next run is one interval after the
previous run was *scheduled*, not after it finished, so probe duration does
not make checks drift into alignment. A run that overruns its next slot (by
waiting for a free slot under the concurrency limit) skips the missed slots
instead of firing back to back, and a check never has two probes in flight.

### Two kinds of stop: stop scheduling, and abort

Stopping a check's schedule and cancelling its probe in flight are separate.
When a check is removed or changed by a reload, or the service shuts down,
its schedule stops but a probe already running finishes and its result is
recorded; a changed check's replacement waits for that probe before its own
first run. Probes are only aborted if shutdown's grace period runs out, and
an aborted probe's result is discarded because it says nothing about the
target.

### Reloads change only what changed

A new config is matched to the running one by check name. Unchanged checks
keep running on their existing schedule, untouched. Changed checks are
replaced, removed ones stop, and new ones start. An invalid config is
rejected as a whole and the previous one stays in effect.

### Deterministic time in tests

Scheduler and config-watcher tests run inside `testing/synctest` bubbles,
where time is simulated: a test covering five minutes of schedule finishes
in milliseconds, assertions can demand exact intervals, and there are no
sleeps to tune. The bubble also fails a test whose goroutines are still
blocked when it ends, which complements goleak.

### History in SQLite, not Postgres or a time-series database

Every probe result is kept for the retention period (35 days by default) in
an embedded SQLite database: it is what the status page shows and what SLOs
are computed from.

The scale decides it. Twenty checks every 30 seconds is under one write a
second and about 1.7 million rows a month, a few hundred megabytes. The
writer benchmarks at around 170,000 results a second, so SQLite has five
orders of magnitude of headroom here.

- **Postgres** would be another service to deploy, upgrade, back up and keep
  healthy, plus a network hop and credentials, for data that only this one
  process reads and writes. Its strengths (concurrent writers, a query
  planner for many users, replication) solve problems this service does
  not have. It becomes the right choice if several monitor instances must
  share one history.
- **A time-series database**, Prometheus included, is built for numeric
  samples at regular intervals, not for individual events carrying a failure
  reason and error text. Computing SLOs from Prometheus would also make the
  status page depend on another system being up (a monitor's status page
  should not), and Prometheus' default 15-day retention is shorter than a
  30-day SLO window. Prometheus still gets every metric for dashboards; it
  just is not the system of record.

The driver is `modernc.org/sqlite`, SQLite translated to pure Go. It is
slower than the cgo-based `mattn/go-sqlite3`, which does not matter at under
one write a second, and in exchange the binary builds with `CGO_ENABLED=0`:
it cross-compiles to arm64 without a C toolchain and runs on a distroless
image with no libc.

### Probes never wait for the disk

A single goroutine owns all writes and batches them into transactions.
Recording a result only puts it on a bounded queue; if the queue is ever
full (the disk has stalled), the result is dropped and counted in
`synthetic_history_dropped_results_total` rather than blocking. Blocking
would stall probes, distort their timings and turn a disk problem into
false alerts about the targets. WAL mode lets the status page read while
the writer writes, and old results are deleted in small chunks so pruning
never holds the write lock for long.

### SLOs are counted in probes and computed from history

A check's availability is the fraction of its probes in the SLO window that
succeeded, and its error budget is the number of failures the target allows
over that window: a 99.5% target over 86,400 probes allows 432 failures. The
budget's remaining fraction goes negative once it is overspent, so a dashboard
shows by how much rather than stopping at zero.

Counting probes rather than time keeps the maths exact and simple: every probe
is one equal sample. Time when the monitor itself was down produces no probes,
so it counts neither for nor against a check; the monitor cannot know what
happened while it was not looking.

The service computes SLOs from its own history every 30 seconds and exports
the results as gauges, rather than leaving Grafana to compute them from
counters with PromQL. A 30-day `increase()` needs Prometheus to retain 30 days
of data (its default is 15) and makes the numbers depend on Prometheus having
scraped without gaps. Computed from history, they survive restarts of either
service, and the status page and the dashboard always agree.

A config is rejected if a check's SLO window is longer than the history kept,
since its SLO would silently be computed over less data than it claims.

### Metrics carry only a check label

Every per-check series is labelled with `check` and nothing else that can
change; a check's type lives on `synthetic_check_info`. If `type` were a label
on every series, a reload changing a check from TCP to HTTP would leave the
TCP series behind, frozen at their last value. When a check is removed, its
series are deleted once its last probe has finished; deleting them at the
moment of the reload would let that probe recreate them, stale for good.
Failure reasons are a fixed set of ten, so label cardinality stays bounded.
