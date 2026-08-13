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
[Webhook credentials](security.md#webhook-credentials)). It sends JSON, or a
formatted Slack message when the endpoint's *host* is Slack:

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

From the controller:

- `kube_autopsy_reports_created_total` (Counter; `namespace`, `node`, `reason`) — crash volume.
- `kube_autopsy_oom_events_total` (Counter; `namespace`, `node`) — OOM kills detected.
- `kube_autopsy_victim_anon_rss_bytes` (Histogram; `namespace`, `container`) — the victim's anonymous RSS.
- `kube_autopsy_trigger_processes_total` (Counter; `comm`) — which applications trigger OOMs.
- `kube_autopsy_report_age_seconds` (Histogram) — age at GC, so you can tell whether `--ttl-hours` discards reports before anyone reads them.

From the agent:

- `kube_autopsy_capture_latency_seconds` (Histogram) — kernel event to report written.
- `kube_autopsy_log_capture_failures_total` (Counter; `namespace`) — log tails lost to the runtime tearing down first.
- `kube_autopsy_reports_suppressed_total` (Counter) — crashes dropped by the cooldown, i.e. deliberately rather than lost.
- `kube_autopsy_report_errors_total` (Counter; `stage`) — crashes not recorded at all, by failing stage.

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
