# Reference

Everything you can set, every endpoint and every metric.

## Flags

| Flag | Default | Purpose |
|---|---|---|
| `-config` | (required) | checks file, see [`examples/checks.yaml`](../examples/checks.yaml) |
| `-listen` | `:8080` | HTTP server address |
| `-history` | `history.db` | SQLite file of probe results |
| `-history-retention-days` | `35` | days of results kept; must cover every SLO window. Daily totals and incidents are kept 90 days either way |
| `-max-concurrent-probes` | `64` | cap on probes in flight across all checks |
| `-config-poll-interval` | `5s` | how often the checks file is checked for changes |
| `-shutdown-timeout` | `30s` | how long probes in flight get to finish after SIGTERM |
| `-log-level` | `info` | `debug` also logs every successful probe |
| `-debug-listen` | (off) | address for `/debug/pprof/`, e.g. `127.0.0.1:6060`; never expose it publicly |
| `-watchdog-interval` | `1m` | how often to request `WATCHDOG_URL` |

## Environment

| Variable | Purpose |
|---|---|
| `NTFY_URL` | topic to publish to, e.g. `https://ntfy.sh/<long-random-topic>`; without it, notifications are only logged |
| `NTFY_TOKEN` | optional bearer token, for protected topics or self-hosted servers |
| `HEARTBEAT_TOKEN` | token jobs send to ping heartbeat checks; required if there are any |
| `WATCHDOG_URL` | URL at an outside service, such as a [healthchecks.io](https://healthchecks.io) ping URL, requested every minute while the monitor runs, so that service alerts when the monitor goes silent |

## Reloads and shutdown

Edits to the checks file are applied within one poll interval, or at once on
`SIGHUP`. An invalid file is rejected and logged, and the previous checks keep
running. On `SIGTERM` or Ctrl-C, `/readyz` turns unready, no new probes start,
and probes in flight get `-shutdown-timeout` to finish. Their results and
notifications then get up to 5 more seconds to be written before the process
exits.

## Endpoints

| Endpoint | Purpose |
|---|---|
| `GET /` | status page |
| `GET /api/status` | the status page's data as JSON: each check's state, last probe, SLO and daily totals, the incidents of the last 90 days, and the running build's `version` (its commit) |
| `GET /metrics` | Prometheus metrics |
| `GET /healthz` | liveness: the process is up |
| `GET /readyz` | readiness: `503` once shutdown has started |
| `POST /heartbeat/{check}` | ping a heartbeat check, with `Authorization: Bearer $HEARTBEAT_TOKEN`; `204` once saved |

## Metrics

Per-check series are labelled only with `check`; join on
`synthetic_check_info` for a check's `type`.

| Metric | Type | Meaning |
|---|---|---|
| `synthetic_probes_total{check,result}` | counter | probes completed; `result` is `success` or the failure reason |
| `synthetic_probe_success{check}` | gauge | 1 if the last probe succeeded |
| `synthetic_probe_duration_seconds{check}` | histogram | probe duration |
| `synthetic_probe_http_phase_duration_seconds{check,phase}` | histogram | HTTP time by phase: `dns`, `connect`, `tls`, `processing`, `transfer` |
| `synthetic_tls_cert_expiry_timestamp_seconds{check}` | gauge | when the certificate chain stops verifying |
| `synthetic_heartbeat_last_ping_timestamp_seconds{check}` | gauge | when a heartbeat check was last pinged |
| `synthetic_check_info{check,type}` | gauge | 1 per configured check |
| `synthetic_slo_target_ratio{check}` | gauge | SLO target, e.g. `0.995` |
| `synthetic_slo_availability_ratio{check}` | gauge | fraction of probes in the SLO window that succeeded |
| `synthetic_slo_error_budget_remaining_ratio{check}` | gauge | error budget left; negative once overspent |
| `synthetic_slo_burn_rate{check,window}` | gauge | how fast the error budget was spent over `window` (`5m`, `30m`, `1h`, `6h`); 1 would spend exactly all of it over the SLO window |
| `synthetic_scheduler_lag_seconds` | histogram | how late probes started, mostly waiting for a free slot |
| `synthetic_scheduler_skipped_runs_total{check}` | counter | runs skipped because an earlier probe overran |
| `synthetic_probes_in_flight` | gauge | probes running now |
| `synthetic_config_reloads_total{result}` | counter | config loads by `success` or `failure` |
| `synthetic_config_last_reload_successful` | gauge | 0 while a rejected config is being ignored |
| `synthetic_config_last_reload_success_timestamp_seconds` | gauge | when a config was last loaded or reloaded successfully |
| `synthetic_history_dropped_results_total` | counter | results not stored because the write queue was full |
| `synthetic_history_write_errors_total` | counter | failed history writes |
| `synthetic_alert_firing{check}` | gauge | 1 from the alert until the recovery notice, otherwise 0 |
| `synthetic_notifications_delivered_total{kind}` | counter | notifications accepted by ntfy: `alert`, `recovery`, `removed` |
| `synthetic_notification_delivery_failures_total{kind,permanent}` | counter | failed deliveries; `permanent="true"` ones were given up on |
| `synthetic_notifications_pending` | gauge | notifications saved but not yet delivered |
| `synthetic_alert_dropped_results_total` | counter | results not evaluated for alerting because its queue was full |
| `synthetic_watchdog_pings_total{result}` | counter | requests to `WATCHDOG_URL`: `success` or `failure` |

Failure reasons: `timeout`, `dns`, `connect`, `tls`, `cert_expiry`,
`http_status`, `http_body`, `dns_answer`, `latency`, `heartbeat_missed`,
`other`.
