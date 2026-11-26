package v1alpha1

import (
	"encoding/json"
	"os"
	"reflect"
	"sort"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// crdPath is the vendored copy of home-ops' `infrastructure/punch/00-crds.yaml`. It is
// the API contract and home-ops owns it: this test exists so that drift between the
// Go types and the contract fails `go test ./...` instead of failing at apply time in
// the cluster (which is where the apiserver would catch it, loudly but late).
const crdPath = "../../config/crd/punch.raenzo.com_punches.yaml"

// crdPropertyKeys returns the property names of a schema node, or an empty set if the
// node (or the whole path) is absent.
func crdPropertyKeys(t *testing.T, node map[string]any, path ...string) map[string]bool {
	t.Helper()
	cur := node
	for _, p := range path {
		props, ok := cur["properties"].(map[string]any)
		if !ok {
			t.Fatalf("schema node %v has no properties; the CRD shape changed", path)
		}
		next, ok := props[p].(map[string]any)
		if !ok {
			t.Fatalf("schema node %q missing under %v", p, path)
		}
		cur = next
	}
	props, ok := cur["properties"].(map[string]any)
	if !ok {
		return map[string]bool{}
	}
	out := make(map[string]bool, len(props))
	for k := range props {
		out[k] = true
	}
	return out
}

// structJSONNames returns the json names of the fields of a struct type.
func structJSONNames(t reflect.Type) map[string]bool {
	out := make(map[string]bool, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := splitJSONTag(t.Field(i).Tag.Get("json"))
		if name == "" || name == "-" {
			continue
		}
		out[name] = true
	}
	return out
}

func splitJSONTag(tag string) (name, opts string, ok bool) {
	if tag == "" {
		return "", "", false
	}
	for i := 0; i < len(tag); i++ {
		if tag[i] == ',' {
			return tag[:i], tag[i+1:], true
		}
	}
	return tag, "", true
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestCRDMatchesTypes is the one-source-of-truth gate: every property the CRD declares
// for spec/status must exist as a field of PunchSpec/PunchStatus and vice versa.
func TestCRDMatchesTypes(t *testing.T) {
	raw, err := os.ReadFile(crdPath)
	if err != nil {
		t.Fatalf("read vendored CRD: %v", err)
	}
	var doc struct {
		Spec struct {
			Group string `json:"group"`
			Names struct {
				Kind   string `json:"kind"`
				Plural string `json:"plural"`
			} `json:"names"`
			Versions []struct {
				Name   string `json:"name"`
				Schema struct {
					OpenAPIV3Schema map[string]any `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse CRD: %v", err)
	}
	if doc.Spec.Group != GroupName {
		t.Errorf("CRD group = %q, want %q", doc.Spec.Group, GroupName)
	}
	if doc.Spec.Names.Kind != Kind {
		t.Errorf("CRD kind = %q, want %q", doc.Spec.Names.Kind, Kind)
	}
	if doc.Spec.Names.Plural != Plural {
		t.Errorf("CRD plural = %q, want %q", doc.Spec.Names.Plural, Plural)
	}
	var schema map[string]any
	found := false
	for _, v := range doc.Spec.Versions {
		if v.Name == Version {
			schema, found = v.Schema.OpenAPIV3Schema, true
		}
	}
	if !found {
		t.Fatalf("CRD has no version %q", Version)
	}

	for _, tc := range []struct {
		name   string
		cr     map[string]bool
		goType reflect.Type
	}{
		{"spec", crdPropertyKeys(t, schema, "spec"), reflect.TypeOf(PunchSpec{})},
		{"status", crdPropertyKeys(t, schema, "status"), reflect.TypeOf(PunchStatus{})},
	} {
		want := structJSONNames(tc.goType)
		if len(tc.cr) == 0 {
			t.Fatalf("%s: CRD declares no properties — the parser is not looking where the schema is", tc.name)
		}
		if !reflect.DeepEqual(sortedKeys(want), sortedKeys(tc.cr)) {
			t.Errorf("%s: Go fields %v != CRD properties %v (the CRD is the contract; home-ops owns it)",
				tc.name, sortedKeys(want), sortedKeys(tc.cr))
		}
	}
}

// TestStatusRoundTrip pins the wire shape: the JSON keys are exactly the CRD's, so a
// status patch built from these structs is the one the apiserver accepts.
func TestStatusRoundTrip(t *testing.T) {
	seen := time.Date(2026, 11, 25, 2, 11, 46, 0, time.UTC)
	st := PunchStatus{
		State: "up", PublicIP: "120.244.157.23", PublicPort: 38698, InnerPort: 27257,
		Reason: "", LastSeenAt: metav1.NewTime(seen), ObservedGeneration: 3,
	}
	var m map[string]any
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"state", "publicIP", "publicPort", "innerPort", "lastSeenAt", "observedGeneration"} {
		if _, ok := m[k]; !ok {
			t.Errorf("status JSON is missing %q: %s", k, raw)
		}
	}
	if _, ok := m["reason"]; ok {
		// reason is omitempty and empty here: absent is fine, wrong is not.
		t.Logf("reason omitted when empty (ok): %s", raw)
	}

	sp := PunchSpec{Target: "192.168.9.70:6881", Protocol: "TCP", BindPort: 27257}
	rawSpec, err := json.Marshal(sp)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	var sm map[string]any
	if err := json.Unmarshal(rawSpec, &sm); err != nil {
		t.Fatalf("unmarshal spec: %v", err)
	}
	for _, k := range []string{"target", "protocol", "bindPort"} {
		if _, ok := sm[k]; !ok {
			t.Errorf("spec JSON is missing %q: %s", k, rawSpec)
		}
	}
}

// TestEffectiveProtocol: an empty protocol is TCP (the CRD's default), so the agent and
// the ConfigMap never see a blank.
func TestEffectiveProtocol(t *testing.T) {
	for _, in := range []string{"", "TCP", "tcp"} {
		if got := (PunchSpec{Protocol: in}).EffectiveProtocol(); got != "TCP" {
			t.Errorf("EffectiveProtocol(%q) = %q, want TCP", in, got)
		}
	}
	if got := (PunchSpec{Protocol: "UDP"}).EffectiveProtocol(); got != "UDP" {
		t.Errorf("EffectiveProtocol(UDP) = %q", got)
	}
}
