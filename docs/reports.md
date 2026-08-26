# Reading reports

## Finding them

Reports register the `pcr` shortname:

```bash
kubectl get pcr

# NAME                      POD          CONTAINER   REASON      EXIT   NODE       PHASE       AGE
# oom-victim-hogger-x7k2p   oom-victim   hogger      OOMKilled   137    node-1     Processed   2m
```

Names are server-generated from a `<pod>-<container>-` prefix, so a container
killed repeatedly gets one report per kill instead of overwriting the previous
one. Because the suffix is opaque, correlate through the labels every report
carries — they select server-side — or through `spec.podName`/`spec.containerName`:

```bash
kubectl get pcr -l autopsy.tty.se/pod=oom-victim         # one pod
kubectl get pcr -A -l autopsy.tty.se/node=node-1         # everything that died on a node
kubectl get pcr -A -l autopsy.tty.se/container=hogger    # one container, every namespace
```

`spec.podUID` records which pod incarnation a report came from, since pod names
are reused across recreations.

For anything the labels do not cover, the `spec` and `status` are rich enough for
JSONPath:

```bash
# Reports triggered by a particular process
kubectl get pcr -o jsonpath='{range .items[?(@.status.diagnostics.triggerComm=="java")]}{.metadata.name}{"\n"}{end}'

# Victim RSS across the cluster, in MB
kubectl get pcr -o custom-columns=NAME:.metadata.name,POD:.spec.podName,VICTIM_RSS_MB:.status.diagnostics.victimRssBytes \
  | awk 'NR>1 {$3=int($3/1024/1024)" MB"; print} NR==1 {print}'
```

## A full report

```bash
kubectl get pcr oom-victim-hogger-x7k2p -o yaml
```

```yaml
apiVersion: autopsy.tty.se/v1alpha1
kind: PodCrashReport
metadata:
  name: oom-victim-hogger-x7k2p
  namespace: default
  labels:
    autopsy.tty.se/pod: oom-victim
    autopsy.tty.se/container: hogger
    autopsy.tty.se/node: node-1
    autopsy.tty.se/reason: OOMKilled
spec:
  podName: oom-victim
  namespace: default
  containerName: hogger
  nodeName: node-1
  terminationReason: OOMKilled
  exitCode: 137
  podUID: 6f1c0f5e-4a2b-4c3d-9e8f-0a1b2c3d4e5f
  timestamp: "2026-07-18T13:24:28Z"
status:
  phase: Processed
  diagnostics:
    oomContext: ContainerLimit
    triggerComm: "dd"
    triggerPid: 2993784
    oomVictimComm: "sh"
    oomVictimPid: 2993762
    oomScore: 26879
    oomScoreAdj: 996
    victimRssBytes: 63832064
    oomScopeLimitBytes: 67108864
    rssDissection:
      anonRssBytes: 62812160
      fileRssBytes: 1019904
      shmemRssBytes: 0
      swapBytes: 0
      pageTablesBytes: 176128
    lastLogLines:
      - "Allocated block 41 (~1MB each)"
      - "Allocated block 42 (~1MB each)"
      - "Allocated block 43 (~1MB each)"
```

Field meanings are in the README's [What a report contains](../README.md#what-a-report-contains);
how a report moves from `Pending` to `Processed` is in
[Report lifecycle](configuration.md#report-lifecycle).
