// Package v1alpha1 holds the Go mirror of home-ops' `Punch` CRD
// (`punch.punch.raenzo.com/v1alpha1`).
//
// home-ops owns the contract: `infrastructure/punch/00-crds.yaml` is the API document,
// and this package is downstream of it. `config/crd/punch.raenzo.com_punches.yaml` is a
// verbatim copy and `crd_test.go` fails `go test ./...` when the two disagree — which is
// the review gate ADR-0044 asks for (a `controller-gen` diff would be the other way to
// say it; a test is cheaper than a generator and this module does no code generation).
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"strings"
)

const (
	// GroupName is the CRD's API group.
	GroupName = "punch.raenzo.com"
	// Version is the only served and stored version.
	Version = "v1alpha1"
	// Kind is the CRD's kind.
	Kind = "Punch"
	// Plural is the resource name used in URLs and in the dynamic client.
	Plural = "punches"

	// ConfigMapSuffix is part of the contract, not an implementation detail: the
	// controller writes `<name>-punch` and nothing else (ADR-0044). A guard in
	// internal/controller refuses any other name.
	ConfigMapSuffix = "-punch"
	// ConfigMapKey is the single key inside that ConfigMap. Consumers open it by name
	// (`/punch/punch.json`), so a controller writing another key leaves the app blind.
	ConfigMapKey = "punch.json"
)

// State values accepted by `status.state` (the CRD's enum).
const (
	StatePending = "pending"
	StateUp      = "up"
	StateDown    = "down"
)

// ReasonBindPortConflict is the reason two Punches that claim the same `spec.bindPort`
// both carry: there is no admission webhook, so the controller validates uniqueness and
// reports rather than silently choosing another port (ADR-0044, Consequences).
const ReasonBindPortConflict = "bindPortConflict"

// GVR is the group/version/resource the dynamic client talks to.
func GVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: GroupName, Version: Version, Resource: Plural}
}

// GVK is the group/version/kind used in ownerReferences.
func GVK() schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: GroupName, Version: Version, Kind: Kind}
}

// Punch is one declared inbound port. Spec is owned by the app's manifest; Status is
// written by the controller only (through the `punches/status` subresource).
type Punch struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PunchSpec   `json:"spec"`
	Status PunchStatus `json:"status,omitempty"`
}

// PunchSpec is the declaration — the CRD's `spec` object, property for property.
type PunchSpec struct {
	// Target is where the router forwards to, `host:port` on the LAN (e.g.
	// `192.168.9.239:5201`). The outer port is BindPort.
	Target string `json:"target"`
	// Protocol is TCP only (the carrier maps TCP and UDP independently, so a UDP punch
	// would need a second public port no announce can express). Empty means TCP.
	Protocol string `json:"protocol,omitempty"`
	// BindPort is the LAN-side port the router binds and the redirect matches — the ONE
	// copy of the port. Unique cluster-wide; the controller reports a conflict as
	// Down(reason=bindPortConflict) instead of picking another.
	BindPort int32 `json:"bindPort"`
}

// PunchStatus mirrors the CRD's `status` object, property for property.
type PunchStatus struct {
	// State is pending | up | down.
	State string `json:"state,omitempty"`
	// PublicIP is the carrier's public address for this mapping.
	PublicIP string `json:"publicIP,omitempty"`
	// PublicPort is the carrier-assigned public port — an announcement, never a number
	// anything may key on (the carrier does not preserve ports).
	PublicPort int32 `json:"publicPort,omitempty"`
	// InnerPort is the CR's BindPort, echoed back: the port the redirect matches.
	InnerPort int32 `json:"innerPort,omitempty"`
	// Reason is a short machine-readable cause, set while State is not up
	// (`noHeartbeat`-shaped, `bindPortConflict`, the agent's own word).
	Reason string `json:"reason,omitempty"`
	// LastSeenAt is stamped by the CONTROLLER's clock on every received report — never
	// the router's. Liveness is a deadline against this field.
	LastSeenAt metav1.Time `json:"lastSeenAt,omitempty"`
	// ObservedGeneration is the metadata.generation the rest of the status came from.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// PunchList is the dynamic client's decode target for a list.
type PunchList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Punch `json:"items"`
}

// EffectiveProtocol is Protocol with the CRD's default applied, upper-cased: the CRD's
// enum has exactly one legal value, `TCP`.
func (s PunchSpec) EffectiveProtocol() string {
	p := strings.TrimSpace(s.Protocol)
	if p == "" {
		return "TCP"
	}
	return strings.ToUpper(p)
}

// ConfigMapName is the only ConfigMap the controller may write for this Punch.
func (p *Punch) ConfigMapName() string {
	return p.Name + ConfigMapSuffix
}
