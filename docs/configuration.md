# Configuration

Every flag has a `KUBE_AUTOPSY_` environment equivalent — uppercase, dashes to
underscores, so `--ttl-hours` is `KUBE_AUTOPSY_TTL_HOURS`. Precedence is
flag > environment > default, so these can come from a ConfigMap or Secret. Both
subcommands accept the whole set; the column below is what acts on each.

| Flag | Default | Honoured by | Purpose |
|---|---|---|---|
| `--capture-logs` | `false` | agent | Copy the container's final log lines into the report. Read [Log capture and access control](security.md#log-capture-and-access-control) first. |
| `--log-tail-lines` | `50` | agent | Lines captured per container, additionally truncated to 2KiB per line and 64KiB per report. |
| `--report-cooldown-seconds` | `30` | agent | Suppress repeat reports for the same container within this window, so a tight crash loop cannot report forever. `0` disables. |
| `--max-concurrent-reports` | `8` | agent | Reports written at once, so a burst of kills cannot flood the API server. |
| `--pod-owner-reference` | `false` | agent | Own each report by its pod, deleting it with the pod. See [Retention and ownership](#retention-and-ownership). |
| `--ttl-hours` | `24` | controller | Age at which the garbage collector deletes a report. |
| `--webhook-include-logs` | `false` | controller | Let captured log lines leave the cluster in the webhook payload. |
| `--webhook-url` | — | controller | **Deprecated**: readable by anyone who can get the pod spec. Use `KUBE_AUTOPSY_WEBHOOK_URL` from a Secret. |
| `--leader-elect` | `true` | controller | Leader election, so replicas do not both process a report. |
| `--metrics-bind-addr` | `:8443` | both | Address for `/metrics`; the quickstart overlay sets `:8080`. |
| `--metrics-secure` | `true` | both | Serve metrics over TLS, requiring each scrape to authenticate and pass a `SubjectAccessReview`. |
| `--health-probe-bind-addr` | `:8081` | both | Address for health and readiness probes. |

`KUBE_AUTOPSY_WEBHOOK_AUTH_HEADER` has no flag at all: it is a credential, and a
flag value is visible in the pod spec and in process listings.

Values that would silently lose data are rejected at startup — `--ttl-hours`
below 1 would make the collector delete every report on its first pass, and
`--log-tail-lines` below 1 is not a meaningful capture request.

## Setting flags on a running install

The two shipped overlays are end points, not the only options — every difference
between them is a flag, so you can harden one axis at a time. To keep the
quickstart but stop copying logs into reports:

```bash
kubectl -n kube-autopsy patch daemonset kube-autopsy-agent --type=json \
  -p='[{"op":"replace","path":"/spec/template/spec/containers/0/args",
        "value":["agent","--capture-logs=false","--metrics-secure=false","--metrics-bind-addr=:8080"]}]'
```

## Retention and ownership

Retention is time-based: a background collector removes reports older than
`--ttl-hours` (24 by default).

Reports are **not** owned by their pod by default. Owning them would let the
control plane delete the report as soon as the pod is recycled, which for a
rollout or a `restartPolicy: Never` pod is seconds after the crash and long
before anyone reads it. Pass `--pod-owner-reference=true` if you would rather
they disappear with their pod.

## Report lifecycle

The agent creates the report in `Pending`, then attaches diagnostics in a second
write because `status` is a subresource. The controller waits for that write, so
an Event and a webhook are never emitted with no memory figures in them; if the
agent dies in between, the report is processed without diagnostics after two
minutes rather than being stranded.

The controller then sends the webhook, marks the report `Processed` with
`status.notifiedAt`, and records the Event — in that order. Sending before the
transition makes delivery at-least-once rather than at-most-once: a failure is
retried with backoff for 10 minutes from creation, after which the report is
marked `Processed` regardless, so a dead endpoint cannot pin everything in
`Pending`. The cost is that a send which succeeds but whose status write fails is
retried, so a receiver may see a duplicate.
