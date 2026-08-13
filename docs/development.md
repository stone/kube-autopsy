# Development

```bash
make build            # ./bin/kube-autopsy
make fmt vet lint
make test             # unit tests, with -race
make e2e              # end-to-end, in a throwaway kind cluster
```

`make e2e-no-cleanup` keeps the kind cluster for debugging. The suite deploys the
overlay a user would install rather than hand-applying manifests, so it covers
that too; it runs `quickstart` by default, and `OVERLAY=hardened
./test/e2e/run.sh` tests the other, minus the log-tail assertions.

## Generated files

CI fails if a generated file has drifted from its source, so regenerate before
pushing:

| Run | After changing | Produces |
|---|---|---|
| `make generate` | `api/v1alpha1/types.go` | `zz_generated.deepcopy.go`, `deploy/base/crd.yaml` |
| `make generate-bpf` | `internal/agent/bpf/oom.c` | the eBPF objects and Go bindings, per architecture |

Only the Go bindings are compared, not the `.o` files: clang does not emit a
byte-identical object across compiler versions, but the struct layout userspace
depends on must not move silently.

## Kernel BTF fixtures

`make btf-fixtures` fetches the kernel BTF blobs the CO-RE relocation tests use
to prove the program resolves against both `mm_struct` layouts. They are several
MB each, so they are fetched rather than committed, and the tests skip without
them — which is why CI fetches them before `go test`.
