# Design decisions

The full list, with the alternatives and the reasoning. The README has the
short version of the main ones.

## Standard library first

The HTTP server uses `net/http` without a framework: since Go 1.22 `ServeMux`
routes on method and path wildcards, which covers everything this service
needs. Logging uses `log/slog`. A third-party module is added only when the
alternative is reimplementing a well-solved problem, and each one is listed
here with its reason.

## Goroutine leak checks in every test package

`go.uber.org/goleak` (test-only) runs after each package's tests and fails
them if any goroutine they started is still alive. Go 1.27's built-in
`goroutineleak` profile was the stdlib alternative, but it only reports
goroutines blocked forever on a channel or lock that nothing can reach. It
would miss a scheduler loop that keeps ticking after `Stop()`, which is the
leak this service is most likely to have.

## CI actions pinned to commit SHAs

A Git tag such as `v7` can be moved to point at different code, so a
compromised action repository can change what runs in CI without any change
to this repository. Pinning to a full commit SHA makes the workflow run exactly
the reviewed code. Each pin carries a comment with its version for readability.

## Config: strict YAML, one block per check type

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

## HTTP probes open a fresh connection every time

Keep-alives are disabled, so every probe resolves DNS, connects and completes
a TLS handshake, as a new visitor would. With connection reuse, a broken DNS
record or an expired certificate could go unnoticed for as long as an old
connection stayed open, and latency would look better than real users see it.
The cost is a little more work per probe, which is negligible at monitoring
rates. `httptrace` splits each probe's time into DNS, connect, TLS, server
processing and transfer, so a slow probe shows where the time went.

## One wrapper owns timeouts

Each probe type only knows how to do its own work and classify its own
errors. A shared wrapper applies the check's timeout through the context,
measures duration and enforces `max_latency`. When the deadline expires, a
probe can fail with many different errors (an i/o timeout, a cancelled read,
a DNS timeout); the wrapper reports all of them as `timeout`, so the failure
reason does not depend on which step was running at the time. A probe
cancelled by its caller, for example at shutdown, is reported as `canceled`
and is not the target's fault.

## One resolver for every probe, released as soon as a probe gives up

All probes resolve names with Go's own DNS client rather than the platform's
(`PreferGo`). It behaves the same on macOS and in the Linux container, has no
cache, so every probe sends real queries, and honours the probe's context.

The goroutine leak tests found one gap: when a probe times out, Go's resolver
cancels the lookup, but the UDP read in flight is bounded only by its
per-attempt deadline from `resolv.conf` (5 seconds by default). Each timed-out
DNS probe left a goroutine and a socket behind for up to 5 seconds, which
piles up against a dead nameserver. The resolver's dial function now closes
the connection as soon as the lookup's context ends.

## TLS checks fail before a certificate expires, not after

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

## Scheduler: one goroutine per check, a shared limit on probes in flight

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
after every deploy. After that it keeps a fixed cadence: the next run is
one interval after the previous run was *scheduled*, not after it finished,
so probe duration does not make checks drift into alignment. A run that
overruns its next slot (by waiting for a free slot under the concurrency
limit) skips the missed slots instead of firing back to back, and a check
never has two probes in flight.

## Two kinds of stop: stop scheduling, and abort

Stopping a check's schedule and cancelling its probe in flight are separate.
When a check is removed or changed by a reload, or the service shuts down,
its schedule stops but a probe already running finishes and its result is
recorded; a changed check's replacement waits for that probe before its own
first run. Probes are only aborted if shutdown's grace period runs out, and
an aborted probe's result is discarded because it says nothing about the
target. Writing results and notifications afterwards has a deadline of its
own, so a probe that hangs until the grace period ends cannot leave the
others' results unwritten.

## Reloads change only what changed

A new config is matched to the running one by check name. Unchanged checks
keep running on their existing schedule, untouched. Changed checks are
replaced, removed ones stop, and new ones start. An invalid config is
rejected as a whole and the previous one stays in effect.

## Deterministic time in tests

Scheduler and config-watcher tests run inside `testing/synctest` bubbles,
where time is simulated: a test covering five minutes of schedule finishes
in milliseconds, assertions can demand exact intervals, and there are no
sleeps to tune. The bubble also fails a test whose goroutines are still
blocked when it ends, which complements goleak.

## History in SQLite, not Postgres or a time-series database

Every probe result is kept for the retention period (35 days by default) in
an embedded SQLite database: it is what the status page shows and what SLOs
are computed from. The status page's 90 days of history come from daily
totals and recorded incidents, kept alongside
([below](#90-days-of-history-from-daily-totals-and-recorded-incidents)).

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

## Probes never wait for the disk

A single goroutine owns all writes and batches them into transactions.
Recording a result only puts it on a bounded queue; if the queue is ever
full (the disk has stalled), the result is dropped and counted in
`synthetic_history_dropped_results_total` rather than blocking. Blocking
would stall probes, distort their timings and turn a disk problem into
false alerts about the targets. WAL mode lets the status page read while
the writer writes, and old results are deleted in small chunks so pruning
never holds the write lock for long.

## SLOs are counted in probes and computed from history

A check's availability is the fraction of its probes in the SLO window that
succeeded, and its error budget is the number of failures the target allows
over that window: a 99.5% target over 86,400 probes allows 432 failures. The
budget's remaining fraction goes negative once it is overspent, so a dashboard
shows by how much rather than stopping at zero.

Counting probes rather than time keeps the maths exact and simple: every probe
is one equal sample. Time when the monitor itself was down produces no probes,
so it counts neither for nor against a check; the monitor cannot know what
happened while it was not looking.

Until a check's history reaches back a whole window, the figures cover only
the time since its first probe. The status page labels them that way
("Availability since 13:05:12 UTC"), and `/api/status` gives `since`. So a
new check's first incident weighs heavily: in production, a 3-minute drill
an hour after deployment read as 85% availability. That is what was
measured. Labelling the period honestly is better than assuming the
unmeasured part of the window went well, and the figure settles as history
builds.

The service computes SLOs from its own history every 30 seconds and exports
the results as gauges, rather than leaving Grafana to compute them from
counters with PromQL. A 30-day `increase()` needs Prometheus to retain 30 days
of data (its default is 15) and makes the numbers depend on Prometheus having
scraped without gaps. Computed from history, they survive restarts of either
service, and the status page and the dashboard always agree.

A config is rejected if a check's SLO window is longer than the history kept,
since its SLO would silently be computed over less data than it claims.

## Metrics carry only a check label

Every per-check series is labelled with `check` and nothing else that can
change; a check's type lives on `synthetic_check_info`. If `type` were a label
on every series, a reload changing a check from TCP to HTTP would leave the
TCP series behind, frozen at their last value. When a check is removed, its
series are deleted once its last probe has finished; deleting them at the
moment of the reload would let that probe recreate them, stale for good.
Failure reasons are a fixed set of ten, so label cardinality stays bounded.

## One alert per incident: thresholds and a state machine

Each check has a small state machine. `failure_threshold` failed probes in a
row (default 3) raise one alert; failures after that add nothing.
`success_threshold` successful probes in a row (default 2) then send one
recovery notice, and a failure before that starts the count again. So a
single blip pages nobody, and a target flapping between up and down stays in
one alert instead of producing a stream of alerts and recoveries. The names
match the Kubernetes probe fields with the same meaning. The transition
logic is a pure function, tested by replaying sequences such as
`FFFSFSFSFSS`.

Counting consecutive probes alone has a blind spot: a target failing at
random, as when one of two replicas is broken, either never fails enough in
a row to alert or alerts and recovers over and over. The next entry covers
the second way into an incident that closes it.

## A second way into an incident: the error budget burning too fast

A check with an SLO also alerts when its error budget is being spent too
fast, using the two paging rules from Google's SRE workbook:

- **Fast:** failures that would spend 2% of the window's budget in an hour,
  and are still that frequent over the last 5 minutes. For a 99.5% target
  over 30 days that is more than 7.2% of probes failing, a burn rate of 14.4.
- **Slow:** 5% of the budget in 6 hours, still burning over the last 30
  minutes: more than 3% of probes failing, a burn rate of 6.

The long window keeps a bad minute from paging anyone. The short window
makes the alert clear soon after the failures stop. The thresholds follow
from the SLO, scaled to its window, so they need no tuning per check. A rule
also needs `failure_threshold` failures in its long window, so one failure of
a check probed hourly, a 100% failure rate on paper, can't page anyone.

Both triggers feed one incident: it starts on either, and ends only once
`success_threshold` probes in a row pass and no burn rule fires. So a check
failing half its probes at random is one alert and one recovery, not a
stream of both. The alternative, a separate burn alert, would have left the
consecutive-failure alert flapping beside it.

The cost is a later recovery notice. While a rule fires, the incident stays
open until its short window is clean, so the notice can come up to 30
minutes after the check is healthy again. It says when probes started
passing, so the real recovery time isn't lost. The burning flag is saved with
the rest of the alert state, so a restart doesn't end an incident early.

Burn rules are evaluated every 30 seconds, apart from the probe results, so
an outage short enough to alert and recover between two evaluations is still
in the rules' windows at the next one. The alert state therefore remembers
when the check's last incident ended. A burn whose failures all came before
that is ignored rather than raising a second alert for the same outage; one
that also counts failures since opens a new incident, which begins at the
first of them, so incidents never overlap. That memory is saved too, so a
check that has recovered from an incident keeps its row of alert state.

## Notifications go through an outbox

Each state change and the notification it calls for are saved to SQLite in
one transaction. A separate goroutine delivers saved notifications oldest
first, retrying with backoff (1 second, doubling to 5 minutes) until ntfy
accepts each one, and deletes it only then. This is the transactional outbox
pattern, and it rules out the failure modes of the obvious designs:

- Saving the state, then sending: a crash in between records the alert as
  sent when it never was, and it is lost for good.
- Sending, then saving: a crash in between forgets the alert was sent, and
  the next run sends it again.
- Keeping state only in memory: every restart during an incident, such as a
  deploy, would alert again, and a recovery after the restart would never be
  announced.

With the outbox, a restart mid-incident neither re-alerts nor loses the
recovery, and notifications that could not be delivered (ntfy down, network
partition) survive restarts and go out in order once it is reachable. One
duplicate is still possible: if the process dies after ntfy accepts a
notification but before it is deleted, it is sent again on the next start.
Exactly-once delivery over a network is impossible without the receiver
deduplicating, and ntfy does not; at-least-once is the right side to err on
for alerts.

Rate limiting and server errors are retried. Other rejections, such as a
bad token, are permanent and dropped with an error log and metric, so one
undeliverable notification cannot block every later one.

## Removed checks resolve their alerts

A check removed from the config while its alert is firing would otherwise
never recover, leaving an incident open forever. Instead it gets a removal
notice once its last probe has finished, including checks removed while the
monitor was down, which are resolved at the next start.

## The status page is server-rendered and runs no JavaScript

The page is an `html/template` embedded in the binary. Charts are inline SVG
computed on the server, tooltips are plain `title`s, and the page reloads
itself every 30 seconds. Without scripts there is nothing to build or
bundle, it works with JavaScript disabled, and its Content-Security-Policy
can forbid scripts and inline styles outright, which removes cross-site
scripting as a class of bug. The cost is a full reload to refresh and plain
browser tooltips; every value is also in a table ("Recent probes", "Daily
uptime"), so nothing is reachable only by hovering, which phones can't do.

It shows check names, never their targets or raw error messages, because a
public status page should not reveal internal hostnames and addresses.
Failures are described by reason ("Connection failed", "Timed out").

A few details that are easy to get wrong:

- Availability is rounded *down*, so 99.996% shows as 99.995%, never as 100%.
- An exhausted error budget is described in probes ("12 failed probes, 1
  allowed"), because a percentage over a short window reads "overspent by
  35966%".
- The sparkline positions points by percentage of its width instead of
  scaling a `viewBox`, so its 2px line and round end dot stay undistorted at
  any width. Each of the last 60 probes has a fixed slot, so a new check fills
  in from the right rather than stretching three probes across the chart.
- Colours come from a palette validated for colour-vision deficiency in both
  light and dark mode, and status colours always come with an icon and a
  label, never colour alone.

## 90 days of history from daily totals and recorded incidents

Each check on the status page has a bar for each of the last 90 days, and
below the checks is a list of the incidents in those days. Neither was
possible from what was stored before.

**Daily totals.** Probe results are kept 35 days, as long as the SLO
windows need. Instead of keeping them for 90, a `daily` table holds each
check's probes and failures per UTC day, and is kept 90 days. A trigger on
`results` counts each result into its day in the transaction that writes
it, so the totals can't drift from the results, and the migration that
added the table counted the results already there. Counting costs the
writer about 28% (from 190,000 to 138,000 results a second on a laptop),
still far beyond what probes produce. Keeping 90 days of results instead
would make the database 2.5 times bigger and have every page view count a
quarter of a million rows per check.

**Incidents.** Nothing remembered past incidents: a notification is
deleted once it's delivered, and alert state only describes the present.
An `incidents` row is now opened by an alert and closed by its recovery or
removal notice, in the same transaction as the alert state and the outbox,
so the history always agrees with the alerts that went out. An incident
ends when probes began succeeding again, not when the recovery notice went
out, which for a burn incident can be half an hour later. The row also
records whether the burn rule raised it. That's how the page tells "down"
from "failing on and off": a check that fails every probe burns its budget
too, so whether it's burning doesn't tell. Incidents from before the table
existed aren't recorded; checks firing at the time got an open one.

**Colours.** A day's bar is red if the check was down at any point that
day, amber if it was failing on and off, green if it was probed without an
incident, and grey before it was probed. The colour follows incidents, not
uptime, so it matches the alerts, and one failed probe doesn't turn a day
amber; the day's uptime is in its tooltip and the table. The palette's red
and green are nearly the same to deuteranopes (ΔE 4.1), so incident days
are also underlined. Phones get the last 30 days, chosen by CSS, with the
label and uptime to match.

## The image: a static binary on distroless

The Dockerfile compiles on the build machine's own architecture and
cross-compiles for the target (`GOARCH`), so the arm64 image builds on an
amd64 CI runner without emulating arm64 in QEMU, which would be several
times slower. That works because the binary has no C dependencies, thanks
to the pure-Go SQLite driver.

The runtime image is `distroless/static`: the binary, CA certificates (which
HTTPS and TLS probes need), timezone data and a non-root user, and no shell
or package manager. It is about 18 MB, 7 MB compressed, and there is little
in it for a vulnerability scanner to flag or an attacker to use. The user is
set as a number (`65532`), because Kubernetes can only enforce
`runAsNonRoot` when it can see the UID without reading the image's
`/etc/passwd`. Base images are pinned by digest, for the same reason
actions are pinned to SHAs.

## One pod, replaced rather than rolled

The history and the alert state live in one SQLite file with one writer, so
the chart runs exactly one replica with the `Recreate` strategy: during an
upgrade the old pod stops before the new one starts. A rolling update would
briefly run two monitors, each probing and alerting on its own, and both
writing the same database. The cost is a gap of a few seconds without probes
during an upgrade. Running several replicas would need leader election, or
a shared database and a way to agree on who sends each alert, which is far
more machinery than a monitor of this size warrants.

The volume has `helm.sh/resource-policy: keep`, so uninstalling the chart
does not delete 30 days of history; installing the same release again picks
it up.

## Checks change without a restart

The checks are a ConfigMap mounted as a directory. The kubelet updates such
a mount in place when the ConfigMap changes (by swapping a symlink, usually
within a minute), and the monitor's poll picks the change up and applies only
what changed, so probes in flight finish and unchanged checks keep their
schedules. Two common patterns would defeat this: a `subPath` mount, which is
a copy made when the pod starts and never updated, and a `checksum/config`
annotation on the pod, which restarts the pod on every change. The chart uses
neither.

## Pod resources: a memory limit but no CPU limit

A CPU limit makes the kernel throttle the container once it uses its share
of a 100 ms period. A probe that is throttled mid-request takes longer, and
the monitor would report that delay as the target's latency. So the pod
requests a little CPU and has no limit. Memory has a limit, because memory
cannot be throttled, only exhausted: a leak should get this pod restarted,
not starve the rest of the node.

## Shutdown fits inside Kubernetes' grace period

When Kubernetes stops a pod it sends SIGTERM, waits
`terminationGracePeriodSeconds`, then kills it. The monitor gives probes in
flight `-shutdown-timeout` to finish and then up to 5 seconds to write their
results and notifications, so the chart sets the grace period 10 seconds
above the shutdown timeout, derived from the same value so the two cannot
drift apart.

## A pod that can do little besides probing

The container runs as a non-root user with a read-only root filesystem, no
Linux capabilities, no privilege escalation and the runtime's default
seccomp profile. It gets no Kubernetes API token, because it never calls the
API. The only writable places are the history volume and a small `/tmp` for
SQLite's temporary files.

## Secrets are referenced, never passed as values

The chart takes the name of an existing Secret rather than the secret values
themselves. Values files tend to end up in Git, and Helm stores every
release's values in the cluster, so an ntfy topic or a password passed as a
value would leak in both places.

Every key in that Secret becomes an environment variable (`envFrom`), rather
than the chart listing the keys it expects. Checks can refer to any variable
as `${NAME}`, so a fixed list would mean a chart change for every new check
that needs a password. The cost is that anything put in the Secret is
visible to the monitor, so it should be a Secret for the monitor alone.

## Chart values are validated

Helm ignores values it does not recognise, so a typo such as
`serviceMonitor.enable: true` would silently do nothing. The chart's
`values.schema.json` rejects unknown and mistyped values when installing,
in the same spirit as the strict checks file. The checks themselves are left
to the monitor to validate, so there is one set of rules, not two that could
disagree.

## Images and charts built per commit, with plain commands

CI publishes an image for every commit on `main`, tagged with the commit SHA
and never `latest`: a tag that always means the same image makes a
deployment reproducible and a rollback a matter of naming the previous SHA.
The workflow calls `docker buildx` and `helm` directly instead of using
wrapper actions, which keeps third-party code away from the job that holds a
token able to push packages; the BuildKit image is pinned by digest, and
Helm is downloaded at a pinned version and checked against its published
hash. After pushing, the job reads the image back from the registry and
checks that it holds both platforms.

The chart is published from the same run, after the image, as version
`0.1.<run number>` with the commit as its appVersion, which the image tag
defaults to. A commit can't contain its own SHA, so the tag to deploy can't
be written into git by hand; stamping it into the chart at build time gives
each chart version exactly one image, and gives a deployment tool such as
Flux a rising version to follow.

CI holds no cluster credentials: whatever deploys the chart pulls it from
inside the cluster. CI's last job instead waits up to 15 minutes for the
public status page to report the new commit in `/api/status`, so a rollout
that fails, or is rolled back, fails the run. It runs once the `STATUS_URL`
repository variable names a deployment.

## The dashboard: one chart per check, states as the status page names them

Latency gets a small chart per check rather than one chart with a line per
check. Checks differ by orders of magnitude (a 4 ms TCP connect beside an
8-second HTTP request), so a shared axis flattens all but one of them, and
more than a handful of lines would need more colours than can be told apart.
The charts show the mean, computed exactly from the histogram's sum and
count: with a few probes a minute per check, a percentile estimated from
histogram buckets would mostly report where the bucket boundaries are.
Heartbeat checks get no latency chart, since they are pinged rather than
timed; a table beside the SLOs shows how long ago each was last pinged.

The state timeline combines two metrics into the status page's four states:
`2 * alert_firing + 1 - probe_success` is 0 when up, 1 when failing below the
alert threshold, 3 when down and 2 when recovering. Failing and recovering
share a colour, as on the status page; they are told apart by their labels
and by always coming before and after "down". An overspent error budget
reads "Used up" rather than a percentage such as -981%, and every status
colour starts at an explicit value, so a panel with no data is never coloured
as if it were healthy or failing.

## Secrets in checks come from the environment, and never sit in URLs

A check can send headers and a body, and both are templates: `${NAME}` is
replaced by the environment variable `NAME` when the request is sent, so a
password or API key lives in a Kubernetes Secret rather than in the checks
file. The parsed config keeps the templates; only the prober expands them,
just before each request, so a secret never travels through the scheduler,
metrics or alerting, which log and display check configuration.

Variables are refused in URLs. Go's HTTP errors quote the URL
(`Get "https://..."`), and probe errors end up in logs and notifications, so
a key in a query string would leak on the first failure. A variable that is
unset or empty rejects the whole config: an empty secret is a deployment
mistake, and sending it would turn into a confusing stream of 401s instead
of one clear error at startup. Only `${NAME}` is special; a lone `$` is
literal, and `$$` writes one, so ordinary JSON bodies need no escaping.

## Multi-step checks start a new session every run

A `steps` check sends a sequence of requests, such as signing in and then
fetching a page that needs the session. Values pass between steps two ways:
cookies, kept in a cookie jar, and variables extracted from a JSON response
by a dotted path (`data.token`) and used in a later step's headers or body as
`${token}`. Each run gets a new, empty jar, for the same reason each probe
opens a new connection: the check should see what a new visitor sees. A jar
kept between runs would let a session from the last run hide a broken
sign-in. The check fails at the first failing step and names it in the
error. Extraction errors name the path that was missing, never the values
around it, because a response to a sign-in is full of secrets. A run shares
one timeout across all its steps, since the question is whether the journey
as a whole works in time.

## Heartbeat checks: the job reports in, and silence is the failure

Some things cannot be probed: a scheduled job on a private network has no
address to call, and the dangerous failure is not an error but the job
quietly not running. A heartbeat check inverts the probe: the job sends
`POST /heartbeat/{check}` when it finishes, and the check fails once the
last ping is older than its period plus a grace allowance. Missing data is
the alarm, which is exactly what log-based alarms treat as all clear.

The check is still run by the scheduler on its interval, so heartbeats reuse
everything else: thresholds, alerting, SLOs, history and the status page,
where they show when the last ping came instead of a latency. Until the
first ping, the clock starts when the check does, so a job that never pings
fails rather than staying grey forever.

- **The ping is saved before it is acknowledged.** A `204` means the ping is
  in SQLite and survives a restart; if it cannot be saved the job gets a
  `503` and can retry. Without that, a redeploy just after a daily job would
  forget its ping and raise a false alarm a day later.
- **Grace defaults to a tenth of the period, and at least a minute,** which
  absorbs jobs that start late or run long without tuning each check.
- **One bearer token for all pings, checked before the name,** so a caller
  without it cannot discover which checks exist. A token in the URL, the
  common alternative, would end up in access logs.

## A dead man's switch for the monitor itself

A monitor cannot report its own death: if its node, its cluster or its
network fails, every alert it would have sent fails with it. So the monitor
requests `WATCHDOG_URL` every minute, and an outside service such as
healthchecks.io alerts when those requests stop. It is the same idea as
heartbeat checks, pointed the other way.

The requests stop as soon as shutdown begins, so a deploy shows up as a
short gap; the outside service's grace period should cover one (a few
minutes). The URL usually identifies the account, so it is treated as a
secret: Go's HTTP errors quote the URL, and the watchdog logs only the
underlying cause.

## Only the status page and heartbeats are public

The chart's Ingress lists the paths it routes (`/`, `/api/status`,
`/static/` and `/heartbeat/`) rather than routing everything except a few.
`/metrics`, `/healthz` and `/readyz` stay inside the cluster, where
Prometheus and the kubelet reach them directly. A list of allowed paths
fails closed: an endpoint added later is private until someone adds it. A
list of blocked paths fails open: one forgotten entry publishes it.

## Prometheus and Grafana belong to the monitoring project, not the chart

Observing things is this project's job, so its `deploy/flux` installs
Prometheus and Grafana for the whole cluster. Any app joins with a
ServiceMonitor and a dashboard ConfigMap, as the apartment app does. The
chart never bundles them: a cluster usually runs one Prometheus, and a chart
that brought its own would duplicate it.

The stack is configured to match:

- **Selection:** it picks up ServiceMonitors, rules and dashboards from
  every namespace, not only those labelled for its own release.
- **No Alertmanager:** the monitor sends its own alerts.
- **No k3s control-plane targets:** k3s runs those components inside its
  own process, where they can't be scraped, so they would only show as
  permanently missing.
- **No Ingress for Grafana:** it is reached by port-forward through the SSH
  tunnel, which keeps one more login off the internet.

## This repo is a Flux tenant of the cluster repo

The registration lives in the cluster repo, so the cluster's owner decides
what joins. Flux then applies `deploy/flux` from this repo with its own
permissions, the same ones it uses for the cluster repo, so a commit here can
change anything in the cluster. That's acceptable while both repos have one
owner. A cluster shared between teams would give each tenant a service
account limited to its own namespaces (`spec.serviceAccountName` on the
Kustomization).

## Secrets are created by hand, outside git

The Secret holding the ntfy topic, the watchdog URL and the heartbeat token
is created once with kubectl and never committed, encrypted or otherwise.
SOPS or Sealed Secrets would let it live in git, at the cost of a key to
manage and protect. With three values that almost never change, one command
in [production.md](production.md) is simpler. Rebuilding the cluster means running it again.
