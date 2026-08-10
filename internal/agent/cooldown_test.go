package agent

import (
	"testing"
	"time"
)

// A container in a tight OOM loop would otherwise produce one report per kill
// indefinitely — API-server and etcd pressure any unprivileged workload can
// generate.
func TestReportCooldown(t *testing.T) {
	base := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)

	t.Run("suppresses a repeat within the window", func(t *testing.T) {
		c := newReportCooldown(30 * time.Second)

		if !c.allow("container-a", base) {
			t.Fatal("first crash should be reported")
		}
		if c.allow("container-a", base.Add(29*time.Second)) {
			t.Error("repeat within the window should be suppressed")
		}
		if !c.allow("container-a", base.Add(31*time.Second)) {
			t.Error("crash after the window should be reported")
		}
	})

	t.Run("suppression is per container", func(t *testing.T) {
		c := newReportCooldown(30 * time.Second)

		if !c.allow("container-a", base) {
			t.Fatal("first crash should be reported")
		}
		if !c.allow("container-b", base) {
			t.Error("an unrelated container must not be suppressed")
		}
	})

	t.Run("a zero window disables suppression", func(t *testing.T) {
		c := newReportCooldown(0)

		for i := range 3 {
			if !c.allow("container-a", base.Add(time.Duration(i)*time.Millisecond)) {
				t.Errorf("crash %d should be reported when cooldown is disabled", i)
			}
		}
	})

	// Nodes with heavy churn would otherwise accumulate an entry per container
	// for the lifetime of the agent.
	t.Run("expired entries are evicted", func(t *testing.T) {
		c := newReportCooldown(30 * time.Second)

		for i := range 100 {
			c.allow(string(rune('a'+i%26))+string(rune('0'+i/26)), base)
		}
		if len(c.last) == 0 {
			t.Fatal("expected entries to be tracked")
		}

		// A later crash triggers eviction of everything older than the window.
		c.allow("fresh", base.Add(time.Hour))
		if len(c.last) != 1 {
			t.Errorf("expected stale entries to be evicted, %d remain", len(c.last))
		}
	})
}
