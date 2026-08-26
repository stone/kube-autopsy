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
| `--max-concurrent-reconciles` | `4` | controller | Reports processed at once. Webhook delivery blocks the worker running it, so a single worker lets one slow endpoint stall every other report. |
| `--max-reports` | `10000` | controller | Cap on reports kept cluster-wide, oldest deleted first, so a cluster-wide crash loop cannot grow the collection past what the controller can hold. `0` disables it. |
| `--metrics-bind-addr` | `:8443` | both | Address for `/metrics`; the quickstart overlay sets `:8080`. |
| `--metrics-secure` | `true` | both | Serve metrics over TLS, requiring each scrape to authenticate and pass a `SubjectAccessReview`. |
| `--health-probe-bind-addr` | `:8081` | both | Address for `/healthz` and `/readyz`. On the agent, `/readyz` reports whether the kprobe is attached, so a node that is not being traced does not read as healthy. |

`KUBE_AUTOPSY_WEBHOOK_AUTH_HEADER` has no flag at all: it is a credential, and a
flag value is visible in the pod spec and in process listings.
`KUBE_AUTOPSY_WEBHOOK_URL` does have a (deprecated) flag, but the environment
value is applied *after* flags are parsed rather than being registered as the
flag's default — otherwise `--help`, or any mistyped flag, would print the
credential into the container log along with the rest of the usage.

## Values rejected at startup

Anything that would silently lose data, or silently not take effect, fails fast:

* `--ttl-hours` outside 1–87600. Below 1 the collector deletes every report on
  its first pass; above roughly 2.5 million the retention duration overflows and
  goes negative, which does the same thing to an operator who was asking for the
  opposite.
* `--log-tail-lines` outside 1–200. The upper bound is the `PodCrashReport`
  schema's own limit on `lastLogLines`. Exceeding it makes the API server reject
  the status write, and since every diagnostic travels in that one write, the
  memory figures and OOM scores would be lost along with the logs.
* `--max-concurrent-reports` and `--max-concurrent-reconciles` below 1;
  `--report-cooldown-seconds` and `--max-reports` below 0.
* A webhook URL that is not an `http`/`https` URL with a host. A URL that cannot
  be used would otherwise fail once per crash, forever. Whitespace around the
  value is trimmed first, since `--from-file` and `$(cat …)` both leave a
  trailing newline.
* A `KUBE_AUTOPSY_*` variable whose value does not parse. Keeping the default
  silently meant `KUBE_AUTOPSY_CAPTURE_LOGS=yes` left capture off and
  `KUBE_AUTOPSY_METRICS_SECURE=no` left metrics authenticated, with nothing to
  tell the operator the setting had not taken.

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

Retention is time-based, with a count cap as a backstop: a background collector
removes reports older than `--ttl-hours` (24 by default), and then trims the
oldest until at most `--max-reports` (10000) remain. The cap exists because a
TTL bounds how long a report lives but not how many exist at once — a
cluster-wide crash loop could otherwise grow the collection until the controller
could no longer hold it, which is exactly when it needs to stay up. Trimming is
counted by `kube_autopsy_reports_trimmed_total`, so it is never silent.

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
