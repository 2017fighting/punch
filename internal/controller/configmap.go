package controller

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/2017fighting/punch/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
)

// PunchDoc is the content of the single key `punch.json`: natmap's document, which is
// the app-facing surface (#355's qBittorrent sidecar opens `/punch/punch.json` and reads
// `port`; hath's fork reads it under `HATH_PUNCH_FILE`). Field names and the lowercase
// protocol are part of the contract in home-ops/infrastructure/punch/00-crds.yaml.
type PunchDoc struct {
	IP        string `json:"ip"`
	Port      int32  `json:"port"`
	InnerPort int32  `json:"inner_port"`
	Protocol  string `json:"protocol"`
}

// Encode renders the document the way natmap writes it: one compact JSON object.
func (d PunchDoc) Encode() (string, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// ConfigMapNameFor is the name rule of the contract: exactly `<punch>-punch`, and
// nothing else. It refuses inputs that cannot produce a legal object name, so the guard
// covers both directions — a caller error and an impossible declaration.
func ConfigMapNameFor(punchName string) (string, error) {
	name := strings.TrimSpace(punchName)
	if name == "" {
		return "", fmt.Errorf("refusing to derive a ConfigMap name from an empty punch name")
	}
	cm := name + v1alpha1.ConfigMapSuffix
	if errs := validation.IsDNS1123Subdomain(cm); len(errs) > 0 {
		return "", fmt.Errorf("punch %q would name ConfigMap %q, which is not a legal object name: %s",
			punchName, cm, strings.Join(errs, "; "))
	}
	return cm, nil
}

// CheckConfigMapName is the code-level guard ADR-0044 requires: the controller must
// refuse to write any ConfigMap whose name is not exactly `<punch>-punch`. RBAC cannot
// express "this name is derived from a Punch", so this is where it is enforced — right
// before the write, on the object that is about to go out.
func CheckConfigMapName(punchName, cmName string) error {
	want, err := ConfigMapNameFor(punchName)
	if err != nil {
		return err
	}
	if cmName != want {
		return fmt.Errorf("refusing to write ConfigMap %q for punch %q: the contract name is %q", cmName, punchName, want)
	}
	return nil
}

// BuildConfigMap renders the `<name>-punch` ConfigMap for an `up` punch: single key
// `punch.json`, ownerReference to the CR (controller: true, so deleting the CR garbage
// collects it), in the CR's own namespace.
func BuildConfigMap(p *v1alpha1.Punch, publicIP string, publicPort int32) (*unstructured.Unstructured, error) {
	name, err := ConfigMapNameFor(p.Name)
	if err != nil {
		return nil, err
	}
	if publicIP == "" || publicPort <= 0 {
		return nil, fmt.Errorf("punch %s/%s: refusing to write a ConfigMap for a mapping without a public tuple (ip=%q port=%d)",
			p.Namespace, p.Name, publicIP, publicPort)
	}
	doc, err := PunchDoc{
		IP:        publicIP,
		Port:      publicPort,
		InnerPort: p.Spec.BindPort,
		Protocol:  strings.ToLower(p.Spec.EffectiveProtocol()),
	}.Encode()
	if err != nil {
		return nil, err
	}

	cm := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      name,
			"namespace": p.Namespace,
			"labels": map[string]any{
				"app.kubernetes.io/name":    "punch",
				"app.kubernetes.io/part-of": "punch",
				"punch.raenzo.com/punch":    p.Name,
			},
			"ownerReferences": []any{map[string]any{
				"apiVersion":         v1alpha1.GVK().GroupVersion().String(),
				"kind":               v1alpha1.Kind,
				"name":               p.Name,
				"uid":                string(p.UID),
				"controller":         true,
				"blockOwnerDeletion": true,
			}},
		},
		"data": map[string]any{
			v1alpha1.ConfigMapKey: doc,
		},
	}}
	// The derived name is asserted again here, on the object itself, so that a future
	// edit to the metadata above cannot publish a name the contract does not allow.
	if err := CheckConfigMapName(p.Name, cm.GetName()); err != nil {
		return nil, err
	}
	return cm, nil
}

// isNotFound reports whether err is an apiserver NotFound, which for a delete means
// "already gone" — the desired end state.
func isNotFound(err error) bool {
	return apierrors.IsNotFound(err)
}
