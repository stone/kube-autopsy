// Package version carries the build identity of the binary.
//
// The values are set at link time by the Makefile and the Dockerfile:
//
//	-X github.com/kube-autopsy/kube-autopsy/internal/version.Version=v0.3.1
//
// A binary built without them reports "dev", which is the honest answer for a
// local `go build`. Without this a running agent could not be tied back to the
// source it came from — the image tag is not enough, since `:latest` moves.
package version

import "runtime/debug"

var (
	// Version is the release tag this binary was built from.
	Version = "dev"
	// Commit is the git revision this binary was built from.
	Commit = "unknown"
)

func init() {
	// A `go build` with no -X flags still records the revision in the build
	// info, so the commit is recoverable even for a developer build.
	if Commit != "unknown" {
		return
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			Commit = s.Value
			return
		}
	}
}

// String renders the build identity for logs and `kube-autopsy version`.
func String() string {
	return Version + " (" + Commit + ")"
}
