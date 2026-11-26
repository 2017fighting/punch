package controller

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/2017fighting/punch/api/v1alpha1"
)

const t0 = "2026-11-25T02:11:46Z"

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return ts
}

func TestEvaluate(t *testing.T) {
	now := mustTime(t, t0)
	const deadline = 60 * time.Second

	tests := []struct {
		name      string
		in        Observed
		wantState string
		wantRsn   string
	}{
		{
			name:      "never reported is pending, not down",
			in:        Observed{Name: "a", Namespace: "ns", BindPort: 1000},
			wantState: v1alpha1.StatePending,
		},
		{
			name:      "agent reported pending keeps its own reason",
			in:        Observed{Name: "a", Namespace: "ns", BindPort: 1000, State: v1alpha1.StatePending, Reason: "natmap has not reported a mapping yet", LastSeenAt: now.Add(-5 * time.Second)},
			wantState: v1alpha1.StatePending,
			wantRsn:   "natmap has not reported a mapping yet",
		},
		{
			name:      "fresh up stands",
			in:        Observed{Name: "a", Namespace: "ns", BindPort: 27257, State: v1alpha1.StateUp, PublicIP: "120.244.157.23", PublicPort: 38698, LastSeenAt: now.Add(-15 * time.Second)},
			wantState: v1alpha1.StateUp,
		},
		{
			name:      "silent past the deadline is down, and the reason counts the silence",
			in:        Observed{Name: "a", Namespace: "ns", BindPort: 27257, State: v1alpha1.StateUp, LastSeenAt: now.Add(-75 * time.Second)},
			wantState: v1alpha1.StateDown,
			wantRsn:   "no status from the punch agent for 75s",
		},
		{
			name:      "exactly at the deadline is still fresh",
			in:        Observed{Name: "a", Namespace: "ns", BindPort: 27257, State: v1alpha1.StateUp, LastSeenAt: now.Add(-deadline)},
			wantState: v1alpha1.StateUp,
		},
		{
			name:      "agent-reported down stays down",
			in:        Observed{Name: "a", Namespace: "ns", BindPort: 27257, State: v1alpha1.StateDown, Reason: "natmap exited: exit status 1", LastSeenAt: now.Add(-2 * time.Second)},
			wantState: v1alpha1.StateDown,
			wantRsn:   "natmap exited: exit status 1",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Evaluate([]Observed{tc.in}, now, deadline)
			if len(got) != 1 {
				t.Fatalf("want 1 result, got %d", len(got))
			}
			if got[0].State != tc.wantState {
				t.Errorf("state = %q, want %q", got[0].State, tc.wantState)
			}
			if got[0].Reason != tc.wantRsn {
				t.Errorf("reason = %q, want %q", got[0].Reason, tc.wantRsn)
			}
		})
	}
}

func TestEvaluateBindPortConflict(t *testing.T) {
	now := mustTime(t, t0)
	fresh := func(name string, port int32) Observed {
		return Observed{Name: name, Namespace: "apps", BindPort: port, State: v1alpha1.StateUp,
			PublicIP: "120.244.157.23", PublicPort: 38698, LastSeenAt: now.Add(-time.Second)}
	}
	got := Evaluate([]Observed{
		fresh("qb", 27257),
		fresh("hath", 27257),
		fresh("speedtest", 27500),
	}, now, 60*time.Second)

	want := map[string]string{"qb": v1alpha1.StateDown, "hath": v1alpha1.StateDown, "speedtest": v1alpha1.StateUp}
	reason := map[string]string{"qb": v1alpha1.ReasonBindPortConflict, "hath": v1alpha1.ReasonBindPortConflict, "speedtest": ""}
	for _, r := range got {
		if r.State != want[r.Name] {
			t.Errorf("%s: state = %q, want %q", r.Name, r.State, want[r.Name])
		}
		if r.Reason != reason[r.Name] {
			t.Errorf("%s: reason = %q, want %q", r.Name, r.Reason, reason[r.Name])
		}
	}
}

func TestEvaluateOrderIsStable(t *testing.T) {
	now := mustTime(t, t0)
	got := Evaluate([]Observed{
		{Name: "b", Namespace: "z"},
		{Name: "a", Namespace: "z"},
		{Name: "a", Namespace: "a"},
	}, now, 60*time.Second)
	var keys []string
	for _, r := range got {
		keys = append(keys, r.Namespace+"/"+r.Name)
	}
	want := []string{"a/a", "z/a", "z/b"}
	if fmt.Sprint(keys) != fmt.Sprint(want) {
		t.Errorf("order = %v, want %v", keys, want)
	}
}

// --- fake cluster helpers ---------------------------------------------------------

func testPunch(name, namespace, target string, bindPort int32) *v1alpha1.Punch {
	return &v1alpha1.Punch{
		TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.GroupName + "/" + v1alpha1.Version, Kind: v1alpha1.Kind},
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace, UID: k8stypes.UID("uid-" + name), Generation: 2,
		},
		Spec: v1alpha1.PunchSpec{Target: target, Protocol: "TCP", BindPort: bindPort},
	}
}

func toUnstructured(t *testing.T, v any) *unstructured.Unstructured {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return &unstructured.Unstructured{Object: m}
}

func newTestController(t *testing.T, now time.Time, deadline time.Duration, punches ...*v1alpha1.Punch) (*Controller, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	scheme := runtime.NewScheme()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			v1alpha1.GVR():                          "PunchList",
			{Version: "v1", Resource: "configmaps"}: "ConfigMapList",
		})
	// Seed through Tracker().Create with the CRD's GVR: the fake's default path derives
	// the resource from the kind by "guess", which turns `Punch` into `punchs` and would
	// make every List in this package mysteriously empty.
	for _, p := range punches {
		if err := client.Tracker().Create(v1alpha1.GVR(), toUnstructured(t, p), p.Namespace); err != nil {
			t.Fatalf("seed punch %s/%s: %v", p.Namespace, p.Name, err)
		}
	}
	c, err := New(Options{Dynamic: client, Deadline: deadline, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, client
}

func getPunch(t *testing.T, c *Controller, ns, name string) v1alpha1.Punch {
	t.Helper()
	punches, err := c.listPunches(t.Context())
	if err != nil {
		t.Fatalf("listPunches: %v", err)
	}
	for _, p := range punches {
		if p.Namespace == ns && p.Name == name {
			return p
		}
	}
	t.Fatalf("punch %s/%s not found", ns, name)
	return v1alpha1.Punch{}
}

func configMapExists(t *testing.T, c *Controller, ns, name string) bool {
	t.Helper()
	_, err := c.dyn.Resource(configMapGVR).Namespace(ns).Get(t.Context(), name, metav1.GetOptions{})
	return err == nil
}

// configMapDoc reads the document out of a ConfigMap, or "" when either is missing.
func configMapDoc(t *testing.T, c *Controller, ns, name string) string {
	t.Helper()
	cm, err := c.dyn.Resource(configMapGVR).Namespace(ns).Get(t.Context(), name, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	return PunchDocOf(cm)
}

// saysDown reports whether the document carries no public tuple, which is how a punch
// says "I am not up" — the file's presence says nothing (see BuildDownConfigMap).
func saysDown(t *testing.T, doc string) bool {
	t.Helper()
	var d PunchDoc
	if err := json.Unmarshal([]byte(doc), &d); err != nil {
		t.Fatalf("document %q is not a punch document: %v", doc, err)
	}
	return d.IP == "" && d.Port == 0
}
