package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/2017fighting/punch/api/v1alpha1"
)

func TestRenderMetrics(t *testing.T) {
	now := mustTime(t, t0)
	seen := now.Add(-15 * time.Second)
	results := Evaluate([]Observed{
		{Name: "qb-private", Namespace: "apps-qbittorrent-private", BindPort: 27257,
			State: v1alpha1.StateUp, LastSeenAt: seen, PublicIP: "120.244.157.23", PublicPort: 38698},
		{Name: "hath", Namespace: "apps-hath", BindPort: 44388, LastSeenAt: now.Add(-5 * time.Minute),
			State: v1alpha1.StateUp},
	}, now, 60*time.Second)

	out := RenderMetrics(results, 7, 3)

	// The contract lines the alerts match on.
	wantLines := []string{
		`punch_last_seen_timestamp_seconds{name="qb-private",namespace="apps-qbittorrent-private"} 1795572691`,
		`punch_state{name="qb-private",namespace="apps-qbittorrent-private",state="up"} 1`,
		`punch_state{name="qb-private",namespace="apps-qbittorrent-private",state="pending"} 0`,
		`punch_state{name="qb-private",namespace="apps-qbittorrent-private",state="down"} 0`,
		`punch_state{name="hath",namespace="apps-hath",state="down"} 1`,
		`punch_agent_reports_total 7`,
		`punch_configmap_writes_total 3`,
	}
	for _, want := range wantLines {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("metrics missing a contract line:\n  %s\nin:\n%s", want, out)
		}
	}

	// A never-reported punch exports timestamp 0 (the freshness alert keys on > 0).
	if !strings.Contains(out, "punch_last_seen_timestamp_seconds{name=\"\",namespace=\"\"} 0") {
		t.Logf("no never-seen series in this fixture (expected)")
	}
	never := RenderMetrics(Evaluate([]Observed{{Name: "fresh", Namespace: "ns"}}, now, time.Minute), 0, 0)
	if !strings.Contains(never, `punch_last_seen_timestamp_seconds{name="fresh",namespace="ns"} 0`) {
		t.Errorf("a never-reported punch must export 0:\n%s", never)
	}

	// Exactly one state series per state per punch, and no family outside the contract.
	families := map[string]bool{}
	stateLines := 0
	for _, line := range strings.Split(out, "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name := line
		if i := strings.IndexAny(line, "{ "); i >= 0 {
			name = line[:i]
		}
		families[name] = true
		if name == "punch_state" {
			stateLines++
		}
	}
	if stateLines != 6 {
		t.Errorf("punch_state lines = %d, want 6 (one-hot, 3 states x 2 punches)", stateLines)
	}
	wantFamilies := map[string]bool{
		"punch_last_seen_timestamp_seconds": true,
		"punch_state":                       true,
		"punch_agent_reports_total":         true,
		"punch_configmap_writes_total":      true,
	}
	for f := range families {
		if !wantFamilies[f] {
			t.Errorf("unexpected metric family %q: the alerts read exactly four names", f)
		}
	}
	for f := range wantFamilies {
		if !families[f] {
			t.Errorf("missing metric family %q", f)
		}
	}

	// HELP/TYPE for every family is what makes a scrape self-describing.
	for _, f := range []string{"punch_last_seen_timestamp_seconds", "punch_state", "punch_agent_reports_total", "punch_configmap_writes_total"} {
		if !strings.Contains(out, "# TYPE "+f+" ") {
			t.Errorf("missing # TYPE for %s", f)
		}
	}
}

func TestRenderMetricsEmptyIsValid(t *testing.T) {
	out := RenderMetrics(nil, 0, 0)
	if strings.Contains(out, "punch_state{") {
		t.Errorf("no punches must mean no per-punch series:\n%s", out)
	}
	if !strings.Contains(out, "punch_agent_reports_total 0") {
		t.Errorf("self-metrics must still be present:\n%s", out)
	}
}
