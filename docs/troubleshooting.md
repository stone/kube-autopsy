# Troubleshooting

**Agent is `CrashLoopBackOff`.** It names each failed precondition, so read the
logs: `kubectl -n kube-autopsy logs -l app.kubernetes.io/component=agent --tail=20`

* `kernel BTF is required for CO-RE but could not be loaded` — the node lacks
  `CONFIG_DEBUG_INFO_BTF`. No workaround short of a kernel that has it.
* `attaching kprobe to oom_kill_process` — the message says whether the symbol is
  missing from `/proc/kallsyms` or has been inlined. If it is present, a
  sandboxed or nested runtime is likely denying `bpf(2)`; the quickstart
  overlay's privileged agent gets past most of those.
* `is not a cgroups v2 (unified) hierarchy` — the node boots the legacy cgroup
  hierarchy. The agent refuses to start rather than run and resolve nothing;
  boot with `systemd.unified_cgroup_hierarchy=1`.
* Failure to load the program at all, before any attach, on a pre-5.8 kernel —
  the ring buffer does not exist there. No workaround on either overlay; see
  [Prerequisites](../README.md#prerequisites).

**Agent pod is `Running` but never `Ready`.** Readiness reports whether the
kprobe is actually attached, not just whether the process started, so this means
the tracer is down. The logs name the reason.

**No report after an OOM kill.** Check the agent on that node. A kill whose
cgroup belongs to no pod — a global OOM that took a systemd unit — is skipped by
design and logged at debug level (`--zap-log-level=debug`). To tell that case
apart from the agent not working, compare
`kube_autopsy_events_received_total` against
`kube_autopsy_report_errors_total{stage="no_pod"}`: if every event lands in
`no_pod`, the agent is receiving kills and resolving none of them. The
`list_pods`, `create` and `status` stages are genuine failures.

**`lastLogLines` is empty.** Log capture is off unless `--capture-logs=true`; the
hardened overlay leaves it off deliberately. See
[Log capture and access control](security.md#log-capture-and-access-control).

**`rssDissection` is missing.** The kernel presented neither `mm_struct` layout;
`kube_autopsy_unsupported_kernel_events_total` counts these. The rest of the
report is still valid.

Linux 6.2 changed how per-process RSS counters are stored; the agent reads both
layouts, so `victimRssBytes` and `rssDissection` work on older kernels (Debian 12,
RHEL 9, Ubuntu 22.04, Amazon Linux 2023) too. If neither is present those fields
are **omitted** rather than zeroed, so an absent value is never mistaken for a
container that used no memory.

**`kubectl get pcr` is empty after a rollout.** Reports are not deleted with
their pod by default, so check `--ttl-hours` (24) instead — see
[Retention and ownership](configuration.md#retention-and-ownership).
