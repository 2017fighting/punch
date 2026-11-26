package controller

import (
	"fmt"
	"sort"
	"time"

	"github.com/2017fighting/punch/api/v1alpha1"
)

// Observed is one Punch as the controller currently knows it: its declaration plus the
// status the agent last made the controller write.
type Observed struct {
	Name       string
	Namespace  string
	BindPort   int32
	State      string // last stored status.state ("" = never reported)
	Reason     string // last stored status.reason
	LastSeenAt time.Time
	PublicIP   string
	PublicPort int32
	InnerPort  int32
}

// Result is what the controller publishes for one Punch: the declaration, the agent's
// last observation, and the VERDICT computed from it.
type Result struct {
	Observed
	State  string
	Reason string
}

// Evaluate computes the state of every Punch at `now`. It is the one place the rules
// live, so the reconcile loop, the status handler and /metrics can never disagree:
//
//   - two Punches sharing a bindPort are both down, reason bindPortConflict — there is
//     no admission webhook, and the controller refuses to pick another port;
//   - never reported (zero lastSeenAt) is pending, not down: a declaration that has not
//     been picked up yet is not a failure, and the agent's own reason is kept;
//   - silent for longer than the deadline is down with a reason naming the silence —
//     liveness is a deadline against the CONTROLLER's clock, because natmap is
//     edge-triggered and publishes no heartbeat (ADR-0044 fact 4);
//   - otherwise the agent's own word stands.
func Evaluate(in []Observed, now time.Time, deadline time.Duration) []Result {
	ports := make(map[int32]int, len(in))
	for _, o := range in {
		if o.BindPort > 0 {
			ports[o.BindPort]++
		}
	}

	out := make([]Result, 0, len(in))
	for _, o := range in {
		r := Result{Observed: o, State: o.State, Reason: o.Reason}
		switch {
		case o.BindPort > 0 && ports[o.BindPort] > 1:
			r.State, r.Reason = v1alpha1.StateDown, v1alpha1.ReasonBindPortConflict
		case o.LastSeenAt.IsZero():
			r.State, r.Reason = v1alpha1.StatePending, o.Reason
		case now.Sub(o.LastSeenAt) > deadline:
			r.State = v1alpha1.StateDown
			r.Reason = fmt.Sprintf("no status from the punch agent for %ds", int(now.Sub(o.LastSeenAt).Seconds()))
		default:
			if r.State == "" {
				r.State = v1alpha1.StatePending
			}
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// ConflictingPorts reports every bindPort claimed by more than one Punch. Exported to
// the status handler, which refuses an `up` report for a conflicting port instead of
// waiting up to a resync for the loop to correct it.
func ConflictingPorts(in []Observed) map[int32]bool {
	counts := make(map[int32]int, len(in))
	for _, o := range in {
		if o.BindPort > 0 {
			counts[o.BindPort]++
		}
	}
	out := make(map[int32]bool)
	for port, n := range counts {
		if n > 1 {
			out[port] = true
		}
	}
	return out
}
