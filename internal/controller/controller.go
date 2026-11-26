// Package controller is the cluster half of the punch: it serves the desired list, turns
// the agent's reports into `Punch` status and into the `<name>-punch` ConfigMap, and
// exports the metrics home-ops' alerts read.
//
// It talks to the apiserver with client-go's DYNAMIC client — no typed clients and no
// controller-runtime — because the only objects it touches are two custom and one core
// resource, and the CRD types here are five fields wide.
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync/atomic"
	"time"

	"github.com/2017fighting/punch/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// Options configures a Controller. Everything is required except Now and Log.
type Options struct {
	Dynamic dynamic.Interface
	// Deadline is how long the controller waits for an agent report before deciding a
	// punch is down (`-deadline`, default 60s = four missed 15s ticks).
	Deadline time.Duration
	Log      *slog.Logger
	Now      func() time.Time
}

// Controller is the HTTP API plus the reconcile loop over `Punch` objects.
type Controller struct {
	dyn      dynamic.Interface
	gvr      schema.GroupVersionResource
	deadline time.Duration
	log      *slog.Logger
	now      func() time.Time

	reports atomic.Int64
	writes  atomic.Int64
}

// New validates the options and returns a Controller.
func New(opts Options) (*Controller, error) {
	if opts.Dynamic == nil {
		return nil, errors.New("controller: a dynamic client is required")
	}
	if opts.Deadline <= 0 {
		return nil, fmt.Errorf("controller: deadline must be positive, got %s", opts.Deadline)
	}
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Controller{
		dyn:      opts.Dynamic,
		gvr:      v1alpha1.GVR(),
		deadline: opts.Deadline,
		log:      log,
		now:      now,
	}, nil
}

// Handler is the whole HTTP surface: the two API endpoints behind the bearer token, and
// /metrics in the clear (it is scraped in-cluster by vmagent; the HTTPRoute does not
// route it).
func (c *Controller) Handler(token string) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/punches/desired", RequireBearer(token, http.HandlerFunc(c.handleDesired)))
	mux.Handle("/punches/status", RequireBearer(token, http.HandlerFunc(c.handleStatus)))
	mux.HandleFunc("/metrics", c.handleMetrics)
	return mux
}

// --- apiserver access -------------------------------------------------------------

func (c *Controller) listPunches(ctx context.Context) ([]v1alpha1.Punch, error) {
	list, err := c.dyn.Resource(c.gvr).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list punches: %w", err)
	}
	out := make([]v1alpha1.Punch, 0, len(list.Items))
	for i := range list.Items {
		p, err := decodePunch(list.Items[i])
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

// decodePunch round-trips through JSON so the CRD's json tags and metav1.Time parsing do
// the work, rather than a hand-written unstructured walk that drifts.
func decodePunch(u unstructured.Unstructured) (v1alpha1.Punch, error) {
	raw, err := json.Marshal(u.Object)
	if err != nil {
		return v1alpha1.Punch{}, fmt.Errorf("encode unstructured punch: %w", err)
	}
	var p v1alpha1.Punch
	if err := json.Unmarshal(raw, &p); err != nil {
		return v1alpha1.Punch{}, fmt.Errorf("decode punch %s/%s: %w", u.GetNamespace(), u.GetName(), err)
	}
	return p, nil
}

func observedFrom(punches []v1alpha1.Punch) []Observed {
	out := make([]Observed, 0, len(punches))
	for _, p := range punches {
		out = append(out, Observed{
			Name:       p.Name,
			Namespace:  p.Namespace,
			BindPort:   p.Spec.BindPort,
			State:      p.Status.State,
			Reason:     p.Status.Reason,
			LastSeenAt: p.Status.LastSeenAt.Time,
			PublicIP:   p.Status.PublicIP,
			PublicPort: p.Status.PublicPort,
			InnerPort:  p.Status.InnerPort,
		})
	}
	return out
}

// patchStatus writes the controller-owned half of a Punch through the `/status`
// subresource, as a merge patch (the RBAC grants `patch`, and a merge patch lets an
// empty field be REMOVED by sending null — which is also how a stale reason is cleared
// when a punch comes back up).
func (c *Controller) patchStatus(ctx context.Context, p *v1alpha1.Punch, ev Result, stampLastSeen bool) error {
	status := map[string]any{
		"state":              ev.State,
		"innerPort":          p.Spec.BindPort,
		"observedGeneration": p.Generation,
	}
	if ev.PublicIP != "" {
		status["publicIP"] = ev.PublicIP
	} else {
		status["publicIP"] = nil
	}
	if ev.PublicPort > 0 {
		status["publicPort"] = ev.PublicPort
	} else {
		status["publicPort"] = nil
	}
	if ev.Reason != "" {
		status["reason"] = ev.Reason
	} else {
		status["reason"] = nil
	}
	if stampLastSeen {
		status["lastSeenAt"] = c.now().UTC().Format(time.RFC3339)
	}
	body, err := json.Marshal(map[string]any{"status": status})
	if err != nil {
		return err
	}
	if _, err := c.dyn.Resource(c.gvr).Namespace(p.Namespace).Patch(
		ctx, p.Name, types.MergePatchType, body, metav1.PatchOptions{}, "status"); err != nil {
		return fmt.Errorf("patch status of punch %s/%s: %w", p.Namespace, p.Name, err)
	}
	return nil
}

// --- ConfigMap --------------------------------------------------------------------

var configMapGVR = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}

// writeConfigMap creates or updates the `<name>-punch` ConfigMap, refusing any other
// name (CheckConfigMapName) and any object that is not in the Punch's own namespace.
func (c *Controller) writeConfigMap(ctx context.Context, punchName, namespace string, cm *unstructured.Unstructured) error {
	if cm.GetNamespace() != namespace {
		return fmt.Errorf("refusing to write ConfigMap %q in namespace %q: the punch lives in %q",
			cm.GetName(), cm.GetNamespace(), namespace)
	}
	if err := CheckConfigMapName(punchName, cm.GetName()); err != nil {
		return err
	}
	cm.SetNamespace(namespace)

	existing, err := c.dyn.Resource(configMapGVR).Namespace(namespace).Get(ctx, cm.GetName(), metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if _, err := c.dyn.Resource(configMapGVR).Namespace(namespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("create configmap %s/%s: %w", namespace, cm.GetName(), err)
		}
	case err != nil:
		return fmt.Errorf("get configmap %s/%s: %w", namespace, cm.GetName(), err)
	default:
		cm.SetResourceVersion(existing.GetResourceVersion())
		if _, err := c.dyn.Resource(configMapGVR).Namespace(namespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("update configmap %s/%s: %w", namespace, cm.GetName(), err)
		}
	}
	c.writes.Add(1)
	c.log.Info("wrote punch configmap", "namespace", namespace, "punch", punchName, "configmap", cm.GetName())
	return nil
}

// writeConfigMapIfChanged writes cm unless the live object already carries the same
// document. The resync runs every 15s and the status endpoint hears from the agent every
// 15s, so an unconditional write would be a write loop over an object whose whole purpose
// is its content — and content is what the mounts project, so that is what decides.
func (c *Controller) writeConfigMapIfChanged(ctx context.Context, punchName, namespace string, cm *unstructured.Unstructured) error {
	existing, err := c.dyn.Resource(configMapGVR).Namespace(namespace).Get(ctx, cm.GetName(), metav1.GetOptions{})
	switch {
	case err != nil && !isNotFound(err):
		return fmt.Errorf("get configmap %s/%s: %w", namespace, cm.GetName(), err)
	case err == nil && PunchDocOf(existing) == PunchDocOf(cm):
		return nil
	}
	return c.writeConfigMap(ctx, punchName, namespace, cm)
}

// writeDownState makes the punch's document say "not up": `down` and `pending` both mean
// "nothing is mapped", and both are SAID rather than implied by absence, because a deleted
// ConfigMap is invisible to a running pod (see BuildDownConfigMap). Idempotent, so the
// resync and every repeated report are free.
func (c *Controller) writeDownState(ctx context.Context, p *v1alpha1.Punch) error {
	cm, err := BuildDownConfigMap(p)
	if err != nil {
		return err
	}
	return c.writeConfigMapIfChanged(ctx, p.Name, p.Namespace, cm)
}

// --- HTTP handlers ----------------------------------------------------------------

type desiredPunch struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Target    string `json:"target"`
	Protocol  string `json:"protocol"`
	BindPort  int32  `json:"bindPort"`
}

type desiredResponse struct {
	Punches []desiredPunch `json:"punches"`
}

// handleDesired serves the full desired set: every Punch in the cluster. It is the
// agent's only view of what should be running, so an error here must never look like an
// empty list — the payload is the answer, and a failure is a non-2xx (the agent keeps
// what it has).
func (c *Controller) handleDesired(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	punches, err := c.listPunches(ctx)
	if err != nil {
		c.log.Error("desired: list punches failed", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	resp := desiredResponse{Punches: make([]desiredPunch, 0, len(punches))}
	for _, p := range punches {
		resp.Punches = append(resp.Punches, desiredPunch{
			Name:      p.Name,
			Namespace: p.Namespace,
			Target:    p.Spec.Target,
			Protocol:  p.Spec.EffectiveProtocol(),
			BindPort:  p.Spec.BindPort,
		})
	}
	c.log.Debug("desired: served", "punches", len(resp.Punches))
	writeJSON(w, http.StatusOK, resp)
}

type statusPunch struct {
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	State      string `json:"state"`
	PublicIP   string `json:"publicIP"`
	PublicPort int32  `json:"publicPort"`
	InnerPort  int32  `json:"innerPort"`
	Reason     string `json:"reason"`
}

type statusRequest struct {
	Punches []statusPunch `json:"punches"`
}

type statusResponse struct {
	Accepted int      `json:"accepted"`
	Skipped  int      `json:"skipped"`
	Problems []string `json:"problems,omitempty"`
}

// handleStatus accepts the agent's full snapshot: one entry per punch per tick. The
// controller stamps `lastSeenAt` from ITS clock (never the agent's), patches the
// `/status` subresource, and writes the `<name>-punch` ConfigMap (its document says
// whether the punch is up).
func (c *Controller) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()

	var req statusRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Validate the whole batch before touching the cluster: a half-applied report is
	// worse than a rejected one, because the agent will send it again.
	for i, s := range req.Punches {
		if s.Name == "" || s.Namespace == "" {
			http.Error(w, fmt.Sprintf("punches[%d]: name and namespace are required", i), http.StatusBadRequest)
			return
		}
		switch s.State {
		case v1alpha1.StatePending, v1alpha1.StateUp, v1alpha1.StateDown:
		default:
			http.Error(w, fmt.Sprintf("punches[%d] (%s/%s): invalid state %q", i, s.Namespace, s.Name, s.State), http.StatusBadRequest)
			return
		}
		if s.State == v1alpha1.StateUp && (s.PublicIP == "" || s.PublicPort <= 0) {
			http.Error(w, fmt.Sprintf("punches[%d] (%s/%s): state up requires publicIP and publicPort", i, s.Namespace, s.Name), http.StatusBadRequest)
			return
		}
	}

	punches, err := c.listPunches(ctx)
	if err != nil {
		c.log.Error("status: list punches failed", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	byKey := make(map[string]*v1alpha1.Punch, len(punches))
	obs := observedFrom(punches)
	for i := range punches {
		byKey[punches[i].Namespace+"/"+punches[i].Name] = &punches[i]
	}
	conflicts := ConflictingPorts(obs)

	var resp statusResponse
	for _, s := range req.Punches {
		p, ok := byKey[s.Namespace+"/"+s.Name]
		if !ok {
			c.log.Warn("status: report for an unknown punch, skipped", "namespace", s.Namespace, "punch", s.Name)
			resp.Skipped++
			resp.Problems = append(resp.Problems, fmt.Sprintf("%s/%s: no such Punch", s.Namespace, s.Name))
			continue
		}

		ev := Result{Observed: Observed{
			Name: p.Name, Namespace: p.Namespace, BindPort: p.Spec.BindPort,
			State: s.State, Reason: s.Reason,
			LastSeenAt: c.now(), PublicIP: s.PublicIP, PublicPort: s.PublicPort,
		}, State: s.State, Reason: s.Reason}

		if conflicts[p.Spec.BindPort] {
			// The declaration is invalid, so the agent's `up` is not the final word:
			// report the conflict and leave the ConfigMap absent rather than publishing
			// a port another punch also claims (ADR-0044: no webhook, so this is the check).
			ev.State, ev.Reason = v1alpha1.StateDown, v1alpha1.ReasonBindPortConflict
			c.log.Warn("status: bindPort conflict, reporting down", "namespace", p.Namespace, "punch", p.Name, "bindPort", p.Spec.BindPort)
		}

		if err := c.patchStatus(ctx, p, ev, true); err != nil {
			c.log.Error("status: patch failed", "namespace", p.Namespace, "punch", p.Name, "err", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		c.reports.Add(1)

		switch ev.State {
		case v1alpha1.StateUp:
			cm, err := BuildConfigMap(p, ev.PublicIP, ev.PublicPort)
			if err != nil {
				c.log.Error("status: cannot build configmap", "namespace", p.Namespace, "punch", p.Name, "err", err)
				break
			}
			if err := c.writeConfigMapIfChanged(ctx, p.Name, p.Namespace, cm); err != nil {
				c.log.Error("status: configmap write failed", "namespace", p.Namespace, "punch", p.Name, "err", err)
			}
		case v1alpha1.StatePending, v1alpha1.StateDown:
			// Both mean "nothing is mapped", and the app learns that from the document:
			// a deleted ConfigMap would leave the last payload in every running pod's
			// volume and say nothing at all.
			if err := c.writeDownState(ctx, p); err != nil {
				c.log.Error("status: down-document write failed", "namespace", p.Namespace, "punch", p.Name, "err", err)
			}
		}
		c.log.Info("status: applied",
			"namespace", p.Namespace, "punch", p.Name, "state", ev.State, "reason", ev.Reason,
			"publicIP", ev.PublicIP, "publicPort", ev.PublicPort, "bindPort", p.Spec.BindPort)
		resp.Accepted++
	}

	writeJSON(w, http.StatusOK, resp)
}

func (c *Controller) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	// The CRs are the state (the controller keeps none), so a scrape lists them and
	// evaluates the same rules the loop uses — a stale cache here would let /metrics and
	// the CRs disagree during exactly the incident the alerts watch.
	punches, err := c.listPunches(ctx)
	if err != nil {
		c.log.Error("metrics: list punches failed", "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	results := Evaluate(observedFrom(punches), c.now(), c.deadline)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(RenderMetrics(results, c.reports.Load(), c.writes.Load())))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// --- reconcile loop ---------------------------------------------------------------

// Reconcile applies the time-based rules to every Punch: silence past the deadline is
// down, a never-reported declaration is pending, a duplicate bindPort is down for both,
// and the ConfigMap exists only while the punch is up. It is idempotent — a no-op tick
// writes nothing.
func (c *Controller) Reconcile(ctx context.Context) error {
	punches, err := c.listPunches(ctx)
	if err != nil {
		return err
	}
	results := Evaluate(observedFrom(punches), c.now(), c.deadline)
	byKey := make(map[string]*v1alpha1.Punch, len(punches))
	for i := range punches {
		byKey[punches[i].Namespace+"/"+punches[i].Name] = &punches[i]
	}

	for _, r := range results {
		p, ok := byKey[r.Namespace+"/"+r.Name]
		if !ok {
			continue
		}
		changed := r.State != p.Status.State || r.Reason != p.Status.Reason
		if changed {
			if err := c.patchStatus(ctx, p, r, false); err != nil {
				c.log.Error("reconcile: patch failed", "namespace", r.Namespace, "punch", r.Name, "err", err)
				continue
			}
			c.log.Info("reconcile: state changed", "namespace", r.Namespace, "punch", r.Name,
				"from", p.Status.State, "to", r.State, "reason", r.Reason,
				"lastSeen", r.LastSeenAt, "deadline", c.deadline)
		}

		switch r.State {
		case v1alpha1.StateDown, v1alpha1.StatePending:
			// Both mean "nothing is mapped", and the document is the only way to say it:
			// a punch that has never been up gets its (down) document too, so an app's
			// start no longer depends on a ConfigMap appearing later.
			if err := c.writeDownState(ctx, p); err != nil {
				c.log.Error("reconcile: down-document write failed", "namespace", r.Namespace, "punch", r.Name, "err", err)
			}
		case v1alpha1.StateUp:
			// Repair as well as recreate: a hand-edited document is not the punch's.
			if r.PublicIP == "" || r.PublicPort <= 0 {
				continue
			}
			cm, err := BuildConfigMap(p, r.PublicIP, r.PublicPort)
			if err != nil {
				continue
			}
			if err := c.writeConfigMapIfChanged(ctx, r.Name, r.Namespace, cm); err != nil {
				c.log.Error("reconcile: configmap write failed", "namespace", r.Namespace, "punch", r.Name, "err", err)
			}
		}
	}
	return nil
}

// Run reconciles every `resync` until the context is cancelled.
func (c *Controller) Run(ctx context.Context, resync time.Duration) {
	if resync <= 0 {
		resync = 15 * time.Second
	}
	ticker := time.NewTicker(resync)
	defer ticker.Stop()
	c.log.Info("reconcile loop started", "resync", resync.String(), "deadline", c.deadline.String())
	for {
		if err := c.Reconcile(ctx); err != nil {
			c.log.Error("reconcile failed", "err", err)
		}
		select {
		case <-ctx.Done():
			c.log.Info("reconcile loop stopped")
			return
		case <-ticker.C:
		}
	}
}
