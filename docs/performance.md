# Performance

How many checks one instance runs, what each probe costs, and what limits it
first. It was measured on two machines:

- **An Apple M1** (8 cores: 4 performance, 4 efficiency), natively on macOS
  and on Linux arm64 in Docker Desktop's VM on the same machine.
- **The production node:** an Oracle Cloud Ampere A1 VM (Neoverse-N1, 2
  cores) running k3s. The benchmarks ran on the host beside the production
  pods, which used about 0.15 cores meanwhile.

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
started) stayed under 100 ms. Each probe type steps up until a level is not
sustained. All probes go to one destination, the worst case for local ports.

```sh
# per probe
go test -run '^$' -bench BenchmarkProbe -benchtime 5000x ./internal/probe

# whole service, with a CPU profile per level
go test -run TestLoad -v -timeout 60m ./cmd/synthetic-monitor -args -load -load-profile /tmp/profiles

# the same on Linux arm64, from a Mac
GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -c -o load.test ./cmd/synthetic-monitor
docker run --rm -v "$PWD":/b alpine:3.22 /b/load.test -test.run TestLoad -test.v -test.timeout 60m -load

# on the production node, copied there with scp: a transient unit, so a
# dropped SSH session doesn't stop it
sudo systemd-run --uid=ubuntu --working-directory="$HOME/bench" \
  -p StandardOutput=file:"$HOME/bench/load.log" "$HOME/bench/load.test" -test.run TestLoad -test.v -load
```

## Per probe

Median of three runs; CPU includes the target server's share.

| Probe | CPU per probe, M1 macOS | CPU per probe, M1 Linux | CPU per probe, Ampere A1 | Allocations | Bytes allocated |
|---|---|---|---|---|---|
| HTTP | 223 µs | 165 µs | 256 µs | 156 | 20 KB |
| HTTPS | 1,330 µs | 1,361 µs | 2,469 µs | 1,033 to 1,037 | 144 to 147 KB |
| TCP | 108 µs | 40 µs | 123 µs | 30 to 42 | 2 KB |
| TLS | 1,159 µs | 1,210 µs | 2,307 µs | 905 to 907 | 124 to 127 KB |
| DNS | 5 µs | 5 µs | 9 µs | 20 | 1 KB |

A Neoverse-N1 core takes about 1.5 times as long as an M1 performance core
for an HTTP probe, and 1.8 to 1.9 times as long for one with a TLS handshake. The allocations
are the same everywhere, as they should be for the same code.

A TLS handshake makes a probe six to ten times more expensive than plain
HTTP, and most of that is the key exchange. Since Go 1.24, TLS negotiates the
post-quantum hybrid X25519MLKEM768 by default; with Go at both ends here, its
ML-KEM half (key generation, encapsulation and decapsulation) costs more than
its X25519 half, and verifying the server's ECDSA signature adds about 7%.
Servers that do not support ML-KEM negotiate plain X25519, so real targets
are somewhat cheaper to probe. Linux's loopback TCP is much cheaper than
macOS's, which shows most in the TCP probe.

## Whole service

### Ampere A1, the production node (2 cores)

| Probe | Checks | Probes/s | Ran | Failed | Lag p50 | Lag p99 | CPU cores | CPU per probe | Heap | Sustained |
|---|---|---|---|---|---|---|---|---|---|---|
| HTTP | 1,000 | 1,000 | 100% | 0 | 0.4 ms | 3.8 ms | 0.47 | 472 µs | 23 MB | yes |
| HTTP | 2,000 | 2,000 | 100% | 0 | 0.2 ms | 6.5 ms | 0.85 | 424 µs | 32 MB | yes |
| HTTP | 4,000 | 3,973 | 99.3% | 0 | 5.1 ms | 149 ms | 1.54 | 387 µs | 69 MB | no: lag, near the CPU limit |
| HTTPS | 500 | 500 | 100% | 0 | 0.3 ms | 6.6 ms | 1.16 | 2.32 ms | 22 MB | yes |
| HTTPS | 1,000 | 734 | 73% | 0 | 749 ms | 1.4 s | 1.64 | 2.23 ms | 38 MB | no: out of CPU |

At 4,000 HTTP checks the node's two cores were close to full: the monitor
used 1.54, production about 0.15, and the kernel's network processing more.
Lag rises steeply as a CPU nears saturation, so the sustainable level lies
between 2,000 and 4,000.

### M1, Linux arm64

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

### M1, macOS

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

From the CPU profiles of the M1 Linux runs, attributing each sample to the
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

## Local ports can run out before CPU

On the 2-core production node, CPU runs out long before local ports do. With
the M1's eight cores, ports can run out first.

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

## Benchmarking beside production

The first two runs on the node were made at the lowest CPU weight
(`CPUWeight=1` on a systemd slice), so that production would always win the
CPU. That made the results wrong. With weight 1 the benchmark, including
its in-process target server, got almost no CPU whenever production briefly
wanted some. Stalls of 130 to 210 ms failed HTTP levels at 1,000 and 2,000
checks while three quarters of the CPU was idle, and the two runs disagreed
on which level failed. At the normal weight the same levels ran with a p99
lag under 7 ms.

Production barely noticed the normal-weight run. The apartment site's own
p99 latency stayed at 5 ms, the monitor's checks of it averaged 18 to 40 ms
except for one minute at 180 ms, and no alerts went out. Its CPU needs were a
few percent of the node, which the normal weight still guarantees many times
over.

The kernel's counters show one more thing worth knowing. Almost every
"retransmitted" segment counted during the runs was a tail loss probe:
Linux resends speculatively when an ACK is late, as ACKs are once the CPU
is saturated. Retransmission timeouts did not change, and the kernel dropped
no packets.

## What it means

Checks normally run every 30 seconds or more. On the M1 under Linux, one
instance's CPU would cover about 720,000 HTTP checks or 150,000 HTTPS checks
at that interval, at around 18 KB of memory per check. The 2-core production
node, shared with the apps it watches, sustains 60,000 HTTP or 15,000 HTTPS
checks at that interval; it runs four. Long before that, the limits
would be the targets, the network, and the operator's patience with that many
alerts.
