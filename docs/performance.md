# Performance

How many checks one instance runs, what each probe costs, and what limits it
first. Everything here was measured on one Apple M1 (8 cores: 4 performance,
4 efficiency), natively on macOS and on Linux arm64 in Docker Desktop's VM on
the same machine. Linux is what production runs; numbers from the Oracle
Ampere A1 node will be added once the service is deployed there.

## Method

**Per probe:** `BenchmarkProbe` runs each probe type against a server in the
same process, 64 probes at a time (the service's default
`-max-concurrent-probes`). The figure that matters is CPU time per probe,
which includes the server's share of the work: a machine can run about
cores ÷ CPU per probe probes a second.

**Whole service:** `TestLoad` wires the same pipeline as the service
(scheduler, real probes, Prometheus metrics, SQLite history and the alert
manager) and runs N checks, each probing every second, against one server
in the same process. Each level runs for 2×MSL plus 5 seconds before being
measured for 10 seconds, so the TIME_WAIT connections it creates have reached
a steady state. A level is *sustained* if at least 99% of the scheduled probes
ran, none failed, and the 99th percentile of scheduling lag (how late a probe
started) stayed under 100 ms. All probes go to one destination, the worst case
for local ports.

```sh
# per probe
go test -run '^$' -bench BenchmarkProbe -benchtime 5000x ./internal/probe

# whole service, with a CPU profile per level
go test -run TestLoad -v -timeout 60m ./cmd/synthetic-monitor -args -load -load-profile /tmp/profiles

# the same on Linux arm64, from a Mac
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -c -o load.test ./cmd/synthetic-monitor
docker run --rm -v "$PWD":/b alpine:3.22 /b/load.test -test.run TestLoad -test.v -test.timeout 60m -load
```

## Per probe

Median of three runs; CPU includes the target server's share.

| Probe | CPU per probe, macOS | CPU per probe, Linux | Allocations | Bytes allocated |
|---|---|---|---|---|
| HTTP | 223 µs | 165 µs | 156 | 20 KB |
| HTTPS | 1,330 µs | 1,361 µs | 1,037 | 147 KB |
| TCP | 108 µs | 40 µs | 30 to 42 | 2 KB |
| TLS | 1,159 µs | 1,210 µs | 907 | 127 KB |
| DNS | 5 µs | 5 µs | 20 | 1 KB |

A TLS handshake makes a probe six to eight times more expensive than plain
HTTP, and most of that is the key exchange. Since Go 1.24, TLS negotiates the
post-quantum hybrid X25519MLKEM768 by default; with Go at both ends here, its
ML-KEM half (key generation, encapsulation and decapsulation) costs more than
its X25519 half, and verifying the server's ECDSA signature adds about 7%.
Servers that do not support ML-KEM negotiate plain X25519, so real targets
are somewhat cheaper to probe. Linux's loopback TCP is much cheaper than
macOS's, which shows most in the TCP probe.

## Whole service

### Linux arm64

| Probe | Checks | Probes/s | Ran | Failed | Lag p50 | Lag p99 | CPU cores | CPU per probe | Heap | Sustained |
|---|---|---|---|---|---|---|---|---|---|---|
| HTTP | 1,000 | 1,000 | 100% | 0 | 0.7 ms | 2.9 ms | 0.71 | 714 µs | 20 MB | yes |
| HTTP | 4,000 | 3,999 | 100% | 0 | 0.5 ms | 3.0 ms | 1.24 | 311 µs | 88 MB | yes |
| HTTP | 8,000 | 8,000 | 100% | 0 | 0.4 ms | 9.6 ms | 1.78 | 223 µs | 127 MB | yes |
| HTTP | 16,000 | 15,998 | 100% | 0 | 0.2 ms | 8.7 ms | 3.23 | 202 µs | 256 MB | yes |
| HTTP | 24,000 | 23,999 | 100% | 0 | 0.3 ms | 76 ms | 5.12 | 213 µs | 432 MB | yes |
| HTTPS | 1,000 | 1,000 | 100% | 0 | 0.4 ms | 2.6 ms | 1.34 | 1.34 ms | 63 MB | yes |
| HTTPS | 3,000 | 3,000 | 100% | 0 | 0.1 ms | 2.8 ms | 3.48 | 1.16 ms | 78 MB | yes |
| HTTPS | 5,000 | 5,010 | 100% | 0 | 0.2 ms | 20 ms | 6.52 | 1.30 ms | 132 MB | yes |
| HTTPS | 6,000 | 5,604 | 93% | 0 | 705 ms | 1.1 s | 7.69 | 1.37 ms | 166 MB | no: out of CPU |

### macOS

| Probe | Checks | Probes/s | Ran | Failed | Lag p50 | Lag p99 | CPU cores | CPU per probe | Heap | Sustained |
|---|---|---|---|---|---|---|---|---|---|---|
| HTTP | 1,000 | 1,000 | 100% | 0 | 0.1 ms | 4.2 ms | 0.68 | 676 µs | 25 MB | yes |
| HTTP | 4,000 | 4,000 | 100% | 0 | 0.04 ms | 26 ms | 0.96 | 240 µs | 66 MB | yes |
| HTTP | 8,000 | 8,000 | 100% | 0 | 0.02 ms | 30 ms | 1.85 | 232 µs | 143 MB | yes |
| HTTP | 12,000 | 5,518 | 46% | 700 | 702 ms | 3.0 s | 2.22 | 402 µs | 295 MB | no: out of local ports |
| HTTPS | 1,000 | 1,000 | 100% | 0 | 0.09 ms | 1.2 ms | 1.04 | 1.04 ms | 51 MB | yes |
| HTTPS | 3,000 | 3,000 | 100% | 0 | 0.04 ms | 1.5 ms | 2.83 | 943 µs | 105 MB | yes |
| HTTPS | 5,000 | 5,000 | 100% | 0 | 0.07 ms | 12 ms | 6.01 | 1.20 ms | 127 MB | yes |
| HTTPS | 6,000 | 5,784 | 96% | 0 | 394 ms | 496 ms | 7.15 | 1.24 ms | 219 MB | no: out of CPU |

CPU per probe falls as load rises because fixed costs (the runtime's
background work, timers waking idle cores) are shared by more probes; from
about 8,000 probes a second it settles at the per-probe cost. Heap grows by
about 18 KB per check.

## Where the CPU goes

From the CPU profiles of the Linux runs, attributing each sample to the
first component found on its stack:

| Component | HTTP, 24,000/s | HTTPS, 5,000/s |
|---|---|---|
| HTTP client and connection setup | 52% | 8% |
| TLS handshake: key exchange (ML-KEM, X25519), signature checks | | 79% |
| Target server (in the same process) | 22% | 1% |
| Go garbage collector | 10% | 9% |
| Go scheduler and network poller | 6% | 2% |
| SQLite history | 3.7% | 0.8% |
| Prometheus metrics | 2.7% | 0.3% |
| Scheduler | 1.5% | 0.2% |
| Alerting | 1.0% | 0.1% |

Under a tenth of the CPU is the monitor's own pipeline; the rest is the work
of probing. A third of all samples are inside the kernel (`Syscall6`):
opening, writing to and closing a fresh TCP connection for every probe, on
both ends. One goroutine and one timer per check cost little: the runtime's
timer bookkeeping (`runtime.(*timers).check`) is about 1% with 24,000 timers
firing a second.

## The first limit is local ports, not CPU

Every probe opens a new connection, and whichever side closes a connection
first keeps its port in TIME_WAIT for 2×MSL (30 seconds on macOS, 60 on
Linux). TCP and TLS probes always close first. HTTP probes ask the server to
close, but in about one connection in eight both sides close at the same
moment and both keep a TIME_WAIT: after 10,000 HTTP probes, 9,828 TIME_WAIT
entries belonged to the server and 1,216 to the monitor.

- **macOS** has 16,384 ephemeral ports for the whole machine. The TCP
  benchmark filled 16,347 of them and then failed with "can't assign
  requested address" (EADDRNOTAVAIL) even when spread over eight
  destinations. In the load test, HTTP at 12,000 checks a second failed with
  connection errors while three quarters of the CPU sat idle.
- **Linux** only needs a port to be unique per destination, has 28,232 of
  them, and (with the default `tcp_tw_reuse = 2`) reuses TIME_WAIT ports on
  loopback. It sustained 24,000 HTTP probes a second to one destination
  without a failure.

Production runs on Linux and probes many different hosts, each with its own
port space, and checks run every 30 seconds or so, so this limit is far away:
on Linux it is roughly 28,000 ports ÷ 60 seconds ≈ 470 monitor-closed
connections a second *to a single destination*, about 14,000 TCP checks on one
host at a 30-second interval.

The monitor could avoid TIME_WAIT entirely by closing with a reset
(`SO_LINGER` of zero), but the targets would see a stream of connection
resets in their logs and metrics. A monitor should look like a well-behaved
client, so probes close gracefully.

One more artefact of benchmarking on loopback: a burst of 64 simultaneous
connections to a test server can overflow its accept queue (TcpExt
`ListenOverflows`). The dropped SYNs are resent after the kernel's one-second
initial retransmission timeout, which can swing the wall-clock throughput of a
sub-second benchmark by an order of magnitude. That is why the per-probe
figures above are CPU time, which does not count the wait, and why each
benchmark warms its server first.

## What it means

Checks normally run every 30 seconds or more. On Linux, one instance's CPU
would cover about 720,000 HTTP checks or 150,000 HTTPS checks at that
interval, at around 18 KB of memory per check. Long before that, the limits
would be the targets, the network, and the operator's patience with that many
alerts.
