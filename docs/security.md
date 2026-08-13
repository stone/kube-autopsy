# Security

## Log capture and access control

The quickstart manifest captures container logs; the hardened one does not, and
the binary's own default is off.

This is about access control, not overhead. Reading a container's logs normally
requires `get pods/log`; copied into a `PodCrashReport`, those lines become
readable by anyone with `get podcrashreports` in the namespace — a weaker
permission that read-only and developer roles often grant via wildcards. Logs
routinely contain bearer tokens, connection strings and personal data.

So if you keep it on, treat `get podcrashreports` as equivalent to `pods/log` for
those namespaces. Log lines leave the cluster only if you also pass
`--webhook-include-logs=true`.

## Component privileges

The agent and controller have separate ServiceAccounts, so compromising the
node-resident agent does not grant the controller's rights — notably deleting
reports, which on a forensics tool means destroying evidence.

| | Agent | Controller |
|---|---|---|
| `pods` | get, list, watch | — |
| `podcrashreports` | create | get, list, watch, update, patch, delete |
| `podcrashreports/status` | update, patch | update, patch |
| `events` | — | create, patch |
| `leases` (namespaced) | — | leader election |

How privileged the agent itself is depends on the manifest. **Hardened** runs it
unprivileged with `CAP_BPF`, `CAP_PERFMON`, `CAP_SYS_RESOURCE`,
`allowPrivilegeEscalation: false` and `seccompProfile: RuntimeDefault`.
**Quickstart runs it `privileged: true`** — every capability, no seccomp, host
devices exposed — which is the reason to move to hardened once the tool is known
to work in your cluster. Both share a read-only root filesystem, no host PID
namespace, and one host mount: `/var/log/pods`, read-only, used only when log
capture is on.

## Webhook credentials

A webhook URL is a credential: whoever holds it can post to your channel. Supply
it via `KUBE_AUTOPSY_WEBHOOK_URL` from a Secret rather than the deprecated
`--webhook-url` flag, which is readable by anyone who can `get pods`. The
controller never logs the URL beyond its host.

```bash
kubectl -n kube-autopsy create secret generic kube-autopsy-webhook \
  --from-literal=url='https://hooks.slack.com/services/...' \
  --from-literal=authHeader='Bearer optional-token'
```

The shipped Deployment already reads that Secret as `optional`, so creating it is
all that is needed.
