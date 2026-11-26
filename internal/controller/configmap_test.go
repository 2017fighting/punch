package controller

import (
	"reflect"
	"strings"
	"testing"

	"github.com/2017fighting/punch/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestConfigMapNameFor(t *testing.T) {
	got, err := ConfigMapNameFor("qb-private")
	if err != nil {
		t.Fatalf("ConfigMapNameFor: %v", err)
	}
	if got != "qb-private-punch" {
		t.Errorf("name = %q, want qb-private-punch", got)
	}
	for _, bad := range []string{"", "  ", strings.Repeat("a", 260)} {
		if _, err := ConfigMapNameFor(bad); err == nil {
			t.Errorf("ConfigMapNameFor(%q) = nil error, want refusal", bad)
		}
	}
}

func TestCheckConfigMapNameRefusesAnythingElse(t *testing.T) {
	if err := CheckConfigMapName("qb-private", "qb-private-punch"); err != nil {
		t.Errorf("the contract name was refused: %v", err)
	}
	for _, other := range []string{"punch", "qb-private", "qb-private-punch.json", "other-punch", ""} {
		if err := CheckConfigMapName("qb-private", other); err == nil {
			t.Errorf("CheckConfigMapName(qb-private, %q) = nil, want refusal", other)
		}
	}
}

func TestBuildConfigMap(t *testing.T) {
	p := testPunch("qb-private", "apps-qbittorrent-private", "192.168.9.70:6881", 27257)
	cm, err := BuildConfigMap(p, "120.244.157.23", 38698)
	if err != nil {
		t.Fatalf("BuildConfigMap: %v", err)
	}
	if cm.GetName() != "qb-private-punch" {
		t.Errorf("name = %q", cm.GetName())
	}
	if cm.GetNamespace() != p.Namespace {
		t.Errorf("namespace = %q, want %q", cm.GetNamespace(), p.Namespace)
	}
	doc, found, err := unstructuredString(cm.Object, "data", v1alpha1.ConfigMapKey)
	if err != nil || !found {
		t.Fatalf("data[%s] missing: found=%v err=%v", v1alpha1.ConfigMapKey, found, err)
	}
	want := `{"ip":"120.244.157.23","port":38698,"inner_port":27257,"protocol":"tcp"}`
	if doc != want {
		t.Errorf("punch.json = %s, want %s", doc, want)
	}
	keys, _, _ := unstructuredKeys(cm.Object, "data")
	if len(keys) != 1 {
		t.Errorf("data has keys %v, want exactly [%s]", keys, v1alpha1.ConfigMapKey)
	}

	refs, found, err := unstructuredSlice(cm.Object, "metadata", "ownerReferences")
	if err != nil || !found || len(refs) != 1 {
		t.Fatalf("ownerReferences: found=%v len=%d err=%v", found, len(refs), err)
	}
	ref := refs[0].(map[string]any)
	for k, v := range map[string]any{
		"apiVersion":         v1alpha1.GroupName + "/" + v1alpha1.Version,
		"kind":               v1alpha1.Kind,
		"name":               p.Name,
		"uid":                "uid-" + p.Name,
		"controller":         true,
		"blockOwnerDeletion": true,
	} {
		if ref[k] != v {
			t.Errorf("ownerReferences[0].%s = %v, want %v", k, ref[k], v)
		}
	}

	if _, err := BuildConfigMap(p, "", 38698); err == nil {
		t.Error("BuildConfigMap with no public IP = nil error, want refusal")
	}
	if _, err := BuildConfigMap(p, "120.244.157.23", 0); err == nil {
		t.Error("BuildConfigMap with port 0 = nil error, want refusal")
	}
}

// TestBuildDownConfigMap: the "not up" object differs from the "up" one in exactly one
// place — the document — because everything else about it is the contract (name, key,
// labels, ownerReference), and a consumer must be able to open it by name either way.
func TestBuildDownConfigMap(t *testing.T) {
	p := testPunch("hath", "apps-hath", "192.168.9.71:44388", 44388)
	cm, err := BuildDownConfigMap(p)
	if err != nil {
		t.Fatalf("BuildDownConfigMap: %v", err)
	}
	up, err := BuildConfigMap(p, "120.244.157.23", 38697)
	if err != nil {
		t.Fatalf("BuildConfigMap: %v", err)
	}
	if cm.GetName() != up.GetName() || cm.GetNamespace() != up.GetNamespace() {
		t.Errorf("down object = %s/%s, up object = %s/%s: the name must not depend on state",
			cm.GetNamespace(), cm.GetName(), up.GetNamespace(), up.GetName())
	}

	doc, found, err := unstructuredString(cm.Object, "data", v1alpha1.ConfigMapKey)
	if err != nil || !found {
		t.Fatalf("data[%s] missing: found=%v err=%v", v1alpha1.ConfigMapKey, found, err)
	}
	want := `{"ip":"","port":0,"inner_port":44388,"protocol":"tcp"}`
	if doc != want {
		t.Errorf("punch.json = %s, want %s", doc, want)
	}
	if !saysDown(t, doc) {
		t.Error("the down document must read as no public tuple")
	}
	// The rest of the object is the up shape's: same labels, same single ownerRef.
	if !reflect.DeepEqual(cm.GetLabels(), up.GetLabels()) {
		t.Errorf("labels = %v, want %v", cm.GetLabels(), up.GetLabels())
	}
	downRefs, _, _ := unstructuredSlice(cm.Object, "metadata", "ownerReferences")
	upRefs, _, _ := unstructuredSlice(up.Object, "metadata", "ownerReferences")
	if !reflect.DeepEqual(downRefs, upRefs) {
		t.Errorf("ownerReferences = %v, want %v", downRefs, upRefs)
	}
}

// TestWriteConfigMapGuard is the code-level guard ADR-0044 asks for: even handed the
// wrong name, the writer refuses before any request reaches the apiserver.
func TestWriteConfigMapGuard(t *testing.T) {
	p := testPunch("qb-private", "apps", "192.168.9.70:6881", 27257)
	c, client := newTestController(t, mustTime(t, t0), 60e9, p)

	good, err := BuildConfigMap(p, "120.244.157.23", 38698)
	if err != nil {
		t.Fatalf("BuildConfigMap: %v", err)
	}

	// 1. a name the contract does not allow
	bad := good.DeepCopy()
	bad.SetName("some-other-configmap")
	if err := c.writeConfigMap(t.Context(), p.Name, p.Namespace, bad); err == nil {
		t.Fatal("writeConfigMap accepted a name that is not <name>-punch")
	}
	// 2. the right name in the wrong namespace (the punch's own namespace is the rule)
	wrongNS := good.DeepCopy()
	wrongNS.SetNamespace("kube-system")
	if err := c.writeConfigMap(t.Context(), p.Name, p.Namespace, wrongNS); err == nil {
		t.Fatal("writeConfigMap accepted the wrong namespace")
	}
	if configMapExists(t, c, "apps", "some-other-configmap") || configMapExists(t, c, "kube-system", "qb-private-punch") {
		t.Fatal("a refused write reached the apiserver")
	}

	// 3. the contract write lands, and a second write is an update rather than a 409
	if err := c.writeConfigMap(t.Context(), p.Name, p.Namespace, good); err != nil {
		t.Fatalf("writeConfigMap: %v", err)
	}
	if !configMapExists(t, c, "apps", "qb-private-punch") {
		t.Fatal("the ConfigMap was not created")
	}
	again, err := BuildConfigMap(p, "120.244.157.24", 40001)
	if err != nil {
		t.Fatalf("BuildConfigMap (2): %v", err)
	}
	if err := c.writeConfigMap(t.Context(), p.Name, p.Namespace, again); err != nil {
		t.Fatalf("second writeConfigMap: %v", err)
	}
	cm, err := client.Resource(configMapGVR).Namespace("apps").Get(t.Context(), "qb-private-punch", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	doc, _, _ := unstructuredString(cm.Object, "data", v1alpha1.ConfigMapKey)
	if !strings.Contains(doc, "40001") {
		t.Errorf("the second write did not update the document: %s", doc)
	}
	if got := c.writes.Load(); got != 2 {
		t.Errorf("punch_configmap_writes_total = %d, want 2", got)
	}
}

// --- small unstructured helpers, kept here so the tests read like the contract ----

func unstructuredString(obj map[string]any, fields ...string) (string, bool, error) {
	var cur any = obj
	for _, f := range fields {
		m, ok := cur.(map[string]any)
		if !ok {
			return "", false, nil
		}
		cur, ok = m[f]
		if !ok {
			return "", false, nil
		}
	}
	s, ok := cur.(string)
	return s, ok, nil
}

func unstructuredKeys(obj map[string]any, fields ...string) ([]string, bool, error) {
	var cur any = obj
	for _, f := range fields {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false, nil
		}
		cur, ok = m[f]
		if !ok {
			return nil, false, nil
		}
	}
	m, ok := cur.(map[string]any)
	if !ok {
		return nil, false, nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out, true, nil
}

func unstructuredSlice(obj map[string]any, fields ...string) ([]any, bool, error) {
	var cur any = obj
	for _, f := range fields {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false, nil
		}
		cur, ok = m[f]
		if !ok {
			return nil, false, nil
		}
	}
	s, ok := cur.([]any)
	return s, ok, nil
}
