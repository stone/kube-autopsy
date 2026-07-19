package bpf

//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -target amd64 -type oom_event Bpf oom.c -- -I/usr/include/bpf
