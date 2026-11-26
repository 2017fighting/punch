package controller

import (
	"strconv"
	"strings"

	"github.com/2017fighting/punch/api/v1alpha1"
)

// metricLabelValues escapes a Prometheus label value (backslash, quote, newline).
func metricLabelValues(v string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return r.Replace(v)
}

// RenderMetrics emits the Prometheus text exposition for the contract home-ops'
// alerts read (`infrastructure/punch/vmrule.yaml`):
//
//	punch_last_seen_timestamp_seconds{namespace,name}  unix seconds, CLUSTER clock, 0 if never seen
//	punch_state{namespace,name,state}                   one-hot over {pending,up,down}
//
// plus the controller's own two counters. Renaming any of the four is a breaking change
// to the alerts, not a cosmetic one. No Prometheus client library: the format is a few
// lines and a dependency here would be larger than the code it replaces.
func RenderMetrics(results []Result, reports, configMapWrites int64) string {
	var b strings.Builder

	b.WriteString("# HELP punch_last_seen_timestamp_seconds Unix seconds, on the controller's own clock, of the last status report received for this Punch; 0 when it has never reported.\n")
	b.WriteString("# TYPE punch_last_seen_timestamp_seconds gauge\n")
	for _, r := range results {
		var ts int64
		if !r.LastSeenAt.IsZero() {
			ts = r.LastSeenAt.Unix()
		}
		b.WriteString("punch_last_seen_timestamp_seconds{name=\"")
		b.WriteString(metricLabelValues(r.Name))
		b.WriteString("\",namespace=\"")
		b.WriteString(metricLabelValues(r.Namespace))
		b.WriteString("\"} ")
		b.WriteString(strconv.FormatInt(ts, 10))
		b.WriteByte('\n')
	}

	b.WriteString("# HELP punch_state The punch's evaluated state, one-hot: 1 for the current state and 0 for the other two (kube-state-metrics shape).\n")
	b.WriteString("# TYPE punch_state gauge\n")
	states := []string{v1alpha1.StatePending, v1alpha1.StateUp, v1alpha1.StateDown}
	for _, r := range results {
		for _, s := range states {
			v := "0"
			if r.State == s {
				v = "1"
			}
			b.WriteString("punch_state{name=\"")
			b.WriteString(metricLabelValues(r.Name))
			b.WriteString("\",namespace=\"")
			b.WriteString(metricLabelValues(r.Namespace))
			b.WriteString("\",state=\"")
			b.WriteString(s)
			b.WriteString("\"} ")
			b.WriteString(v)
			b.WriteByte('\n')
		}
	}

	b.WriteString("# HELP punch_agent_reports_total Punch status entries accepted from the agent since this process started.\n")
	b.WriteString("# TYPE punch_agent_reports_total counter\n")
	b.WriteString("punch_agent_reports_total ")
	b.WriteString(strconv.FormatInt(reports, 10))
	b.WriteByte('\n')

	b.WriteString("# HELP punch_configmap_writes_total `<name>-punch` ConfigMap writes (create or update) since this process started.\n")
	b.WriteString("# TYPE punch_configmap_writes_total counter\n")
	b.WriteString("punch_configmap_writes_total ")
	b.WriteString(strconv.FormatInt(configMapWrites, 10))
	b.WriteByte('\n')

	return b.String()
}
