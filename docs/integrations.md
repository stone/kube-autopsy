# Integrations

## Kubernetes Events

Each processed crash emits an Event on the `PodCrashReport` with reason
`CrashDetected` and type `Warning`, which event-routing tools can forward:
[BotKube](https://botkube.io/) can watch `PodCrashReport` events and relay them
to Slack, Discord and similar; [Robusta](https://robusta.dev/) can trigger
runbooks from them.

## Built-in webhooks

The controller can also dispatch directly, given a URL in the
`kube-autopsy-webhook` Secret (see
[Webhook credentials](security.md#webhook-credentials)). It sends this JSON:

```json
{
  "podName": "oom-victim",
  "namespace": "default",
  "containerName": "hogger",
  "nodeName": "node-1",
  "reason": "OOMKilled",
  "exitCode": 137,
  "timestamp": "2026-07-18T13:24:28Z",
  "victimRssMB": 60,
  "oomScopeLimitMB": 64,
  "lastLogLines": ["…"]
}
```

`lastLogLines` is present only with `--webhook-include-logs`. When the
endpoint's *host* is Slack, a formatted message is sent instead:

```
🚨 Pod Crash Detected
Pod: default/oom-victim (container: hogger)
Node: node-1
Reason: OOMKilled (exit code: 137)
OOM Context: ContainerLimit
Trigger Process: dd (PID: 2993784)
Victim Process: sh (PID: 2993762)
Victim RSS: 60 MB (scope limit: 64 MB)
Time: 2026-07-18T13:24:28Z
```

Delivery is retried; see [Report lifecycle](configuration.md#report-lifecycle).

## Prometheus metrics

Both components expose `/metrics`, through the `kube-autopsy-controller-metrics`
and `kube-autopsy-agent-metrics` Services. Quickstart serves plain HTTP on
`:8080`, scrapeable with no further setup. Hardened serves TLS on `:8443` and
requires each scrape to authenticate and pass a `SubjectAccessReview`, since
serving in the clear exposes namespace, node, container and process names to
anything that can reach the pod:

```bash
kubectl create clusterrolebinding kube-autopsy-prometheus \
  --clusterrole=kube-autopsy-metrics-reader \
  --serviceaccount=monitoring:prometheus
```

The TLS certificate is self-signed and issued for `localhost`, so a scraper
connecting to the pod IP will not verify it. Set `insecureSkipVerify: true` in
the scrape config (the connection is still encrypted, and the
`SubjectAccessReview` is what actually authorises the scrape), or mount your own
certificate.

From the controller:

- `kube_autopsy_reports_created_total` (Counter; `namespace`, `node`, `reason`) — reports processed, i.e. crash volume.
- `kube_autopsy_oom_events_total` (Counter; `namespace`, `node`) — OOM kills detected.
- `kube_autopsy_victim_anon_rss_bytes` (Histogram; `namespace`, `container`) — the victim's anonymous RSS.
- `kube_autopsy_trigger_processes_total` (Counter; `comm`) — which applications trigger OOMs.
- `kube_autopsy_webhook_deliveries_total` (Counter; `result`) — `success`, `retry`, or `dropped`. A non-zero `dropped` rate means crash alerts were abandoned after the retry window and nobody was told.
- `kube_autopsy_webhook_duration_seconds` (Histogram) — delivery latency. Delivery blocks a reconcile worker, so this is also the controller's throughput.
- `kube_autopsy_reports_trimmed_total` (Counter) — reports deleted to honour `--max-reports` rather than because they aged out.
- `kube_autopsy_gc_errors_total` (Counter) — failed deletions. Non-zero means retention is not actually being enforced.
- `kube_autopsy_report_age_seconds` (Histogram) — age at deletion, observed only on a successful delete.

From the agent:

- `kube_autopsy_events_received_total` (Counter) — OOM events read from the kernel, before any filtering. This is the denominator for everything below: without it, "no reports" cannot be told apart from "no kills".
- `kube_autopsy_capture_latency_seconds` (Histogram) — kernel event to report written.
- `kube_autopsy_log_capture_failures_total` (Counter; `namespace`) — log tails lost to the runtime tearing down first.
- `kube_autopsy_reports_suppressed_total` (Counter) — crashes dropped by the cooldown, i.e. deliberately rather than lost.
- `kube_autopsy_unsupported_kernel_events_total` (Counter) — events where the kernel's memory layout was not recognised, so the report carries no RSS breakdown.
- `kube_autopsy_report_errors_total` (Counter; `stage`) — crashes not turned into a report. `no_pod` is expected on a node-level OOM that killed something outside a pod; `list_pods`, `create` and `status` are genuine failures.

### Alerting on the agent not working

The failure mode worth alerting on is silence — an agent that is up but tracing
nothing looks exactly like a quiet node. These two say otherwise:

```yaml
- alert: KubeAutopsyResolvingNothing
  # Every kill received, none attributable to a pod: usually the wrong cgroup
  # hierarchy, or a runtime whose cgroup names are not recognised.
  expr: |
    sum(rate(kube_autopsy_report_errors_total{stage="no_pod"}[15m]))
      / sum(rate(kube_autopsy_events_received_total[15m])) > 0.99
  for: 30m

- alert: KubeAutopsyAlertsDropped
  # The webhook endpoint has been failing long enough that crash alerts are
  # being abandoned.
  expr: increase(kube_autopsy_webhook_deliveries_total{result="dropped"}[1h]) > 0
```

The `comm` and `container` values are chosen by whoever can create pods, so they
pass through a cardinality limiter rather than reaching Prometheus verbatim.

An alert on any crash:

```yaml
apiVersion: monitoring.coreos.com/v1
kind: PrometheusRule
metadata:
  name: kube-autopsy-alerts
spec:
  groups:
  - name: kube-autopsy.rules
    rules:
    - alert: PodCrashDetected
      expr: increase(kube_autopsy_reports_created_total[5m]) > 0
      for: 0m
      labels:
        severity: warning
      annotations:
        summary: "Pod crash detected by kube-autopsy in namespace {{ $labels.namespace }}"
        description: "A pod crash report was generated for reason {{ $labels.reason }} on node {{ $labels.node }}."
```

## k9s

Copy the [plugin](../integrations/k9s/plugins.yml) into your k9s plugin
directory:

```bash
mkdir -p ~/.config/k9s/plugins
curl -fsSL https://raw.githubusercontent.com/stone/kube-autopsy/refs/heads/main/integrations/k9s/plugins.yml \
  -o ~/.config/k9s/plugins/kube-autopsy.yml
```

- `Shift-C` on a **Pod** — open its latest `PodCrashReport`, if any.
- `l` on a **PodCrashReport** — the formatted `lastLogLines`.
- `m` on a **PodCrashReport** — the memory footprint and OOM context.
