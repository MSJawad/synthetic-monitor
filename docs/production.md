# Production

The monitor runs at **https://status.92-5-186-10.sslip.io** and watches
[apartment-finder](https://github.com/MSJawad/apartment-finder), which
shares its cluster: k3s on one Oracle Cloud Always Free Arm VM (2 Ampere
cores, 12 GB), at no cost.

| Check | Every | What a failure means |
|---|---|---|
| `apartment`: GET `/api/listings`, body must contain `"listings":`; SLO 99.5% over 30 days | 30 s | A visitor's first request fails somewhere along sslip.io's DNS, the public IP, Traefik, TLS, the API or Postgres |
| `apartment-cert`, `status-cert`: under 14 days left | 1 h | A certificate's renewal has been failing for over two weeks (cert-manager renews at 30) |
| `sslip-dns`: the site's name still resolves to its IP | 1 min | sslip.io, the free DNS service both names depend on, is failing |

Alerts go to an ntfy topic. The dead man's switch pings healthchecks.io,
which alerts by email and to the same topic.

## How it gets there

The cluster belongs to the apartment-finder repo: its Terraform builds the
VM, and its Flux installs Traefik's configuration, cert-manager and its
apps. This repo joins as a Flux tenant. One GitRepository and Kustomization
in the cluster repo point at [`deploy/flux`](../deploy/flux), which holds:

- [`monitoring.yaml`](../deploy/flux/monitoring.yaml): kube-prometheus-stack
  (Prometheus, Grafana, kube-state-metrics and node-exporter) for the whole
  cluster.
- [`synthetic-monitor.yaml`](../deploy/flux/synthetic-monitor.yaml): the monitor,
  from the chart CI publishes, with the production checks as its values.
  CI renders the chart with these values, and a Go test parses the checks,
  so a mistake fails the commit rather than the rollout.

A push to `main` is live in about 4 minutes. CI builds and publishes the
image and chart in about 3, Flux finds the new chart version within a minute
and upgrades, and CI's last job confirms the new commit on the public status
page. A change to the checks alone reaches the running pod about a minute
after Flux applies it, without a restart. The image CI rebuilds from the
same commit then replaces the pod a few minutes later, which loses nothing:
history and alert state are on the volume.

One Secret is created by hand, once, and never committed:

```sh
# prod.env holds NTFY_URL=... and WATCHDOG_URL=..., one per line and without
# quotes, which kubectl would keep as part of the value.
echo "HEARTBEAT_TOKEN=$(openssl rand -hex 32)" >> prod.env
kubectl -n synthetic-monitor create secret generic synthetic-monitor --from-env-file=prod.env
rm prod.env
```

Grafana has no public address. With the SSH tunnel to the cluster's API open
(see apartment-finder's README), run the following, then open
http://localhost:3000 and log in as `admin` with that password:

```sh
kubectl -n monitoring get secret kube-prometheus-stack-grafana -o jsonpath='{.data.admin-password}' | base64 -d
kubectl -n monitoring port-forward svc/kube-prometheus-stack-grafana 3000:80
```

## Drills

Run against production on 2026-10-22:

- **The apartment API scaled to zero for 3 minutes.** The first probe failed
  29 s into the outage. The alert went out 61 s later, on the third failure
  in a row, at high priority. After the API came back, the first probe
  succeeded 25 s later, and the recovery went out 31 s after that, on the
  second success. Nothing else was sent: one alert, one recovery.
- **The monitor scaled to zero.** healthchecks.io alerted 5 m 12 s later
  (its 1-minute period plus 5 minutes' grace, counted from the last ping).
  It cleared 2 s after the monitor came back, on the monitor's first ping.

## What deploying found

Deploying to production found two problems that no local test had caught:

- **Flux adds build metadata to chart versions.** When Flux pulls a chart
  from an OCI registry, it appends the chart's digest to the version
  (`0.1.17+b3ead67ba00c`). The chart copied that version into the
  `helm.sh/chart` label, where `+` is not allowed, so the API server
  rejected every object. The label now replaces `+`, as charts made by
  `helm create` do. CI also renders the chart with such a version and
  checks every label value against the API server's rule.
- **Init containers reserve CPU for a pod's whole life.** The scheduler
  counts a pod's CPU request as whichever is larger: its biggest init
  container's request, or the sum of its containers'. It holds that amount
  for as long as the pod runs. The apartment's two routing pods each held
  500m for a preparation step that had finished in under a second. So with
  the node at 7% CPU use, all of its CPU was reserved, and the monitor's
  20m pod could not be scheduled. Lowering that request to 50m freed 900m.
