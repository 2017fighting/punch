package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/2017fighting/punch/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const testToken = "fixed-test-bearer-token"

func doRequest(t *testing.T, h http.Handler, method, path string, body string, auth bool) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	if auth {
		r.Header.Set("Authorization", "Bearer "+testToken)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func TestDesiredEndpoint(t *testing.T) {
	p1 := testPunch("qb-private", "apps-qbittorrent-private", "192.168.9.70:6881", 27257)
	p2 := testPunch("hath", "apps-hath", "192.168.9.71:44388", 44388)
	p3 := testPunch("speedtest", "infra", "192.168.9.239:5201", 27500)
	p3.Spec.Protocol = "" // the CRD's default must be resolved for the agent
	c, _ := newTestController(t, mustTime(t, t0), time.Minute, p1, p2, p3)
	h := c.Handler(testToken)

	// The token is the whole credential: the route has no edge auth.
	if rec := doRequest(t, h, http.MethodGet, "/punches/desired", "", false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated desired = %d, want 401", rec.Code)
	}
	rec := doRequest(t, h, http.MethodGet, "/punches/desired", "", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("desired = %d: %s", rec.Code, rec.Body)
	}
	var got desiredResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body)
	}
	if len(got.Punches) != 3 {
		t.Fatalf("punches = %d, want 3", len(got.Punches))
	}
	byName := map[string]desiredPunch{}
	for _, p := range got.Punches {
		byName[p.Name] = p
	}
	qb := byName["qb-private"]
	if qb.Namespace != "apps-qbittorrent-private" || qb.Target != "192.168.9.70:6881" || qb.BindPort != 27257 || qb.Protocol != "TCP" {
		t.Errorf("qb-private = %+v", qb)
	}
	if got := byName["speedtest"].Protocol; got != "TCP" {
		t.Errorf("an empty protocol must resolve to TCP, got %q", got)
	}

	if rec := doRequest(t, h, http.MethodPost, "/punches/desired", "{}", true); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST desired = %d, want 405", rec.Code)
	}
}

func TestStatusEndpointUpWritesStatusAndConfigMap(t *testing.T) {
	now := mustTime(t, t0)
	p := testPunch("qb-private", "apps", "192.168.9.70:6881", 27257)
	c, _ := newTestController(t, now, time.Minute, p)
	h := c.Handler(testToken)

	body := `{"punches":[{"name":"qb-private","namespace":"apps","state":"up","publicIP":"120.244.157.23","publicPort":38698,"innerPort":27257,"reason":""}]}`
	rec := doRequest(t, h, http.MethodPost, "/punches/status", body, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}

	got := getPunch(t, c, "apps", "qb-private")
	if got.Status.State != v1alpha1.StateUp {
		t.Errorf("state = %q", got.Status.State)
	}
	if got.Status.PublicIP != "120.244.157.23" || got.Status.PublicPort != 38698 {
		t.Errorf("public tuple = %s:%d", got.Status.PublicIP, got.Status.PublicPort)
	}
	if got.Status.InnerPort != 27257 {
		t.Errorf("innerPort = %d, want the CR's bindPort", got.Status.InnerPort)
	}
	if got.Status.ObservedGeneration != p.Generation {
		t.Errorf("observedGeneration = %d, want %d", got.Status.ObservedGeneration, p.Generation)
	}
	if !got.Status.LastSeenAt.Time.Equal(now) {
		t.Errorf("lastSeenAt = %s, want the controller's clock %s", got.Status.LastSeenAt.Time, now)
	}
	if !configMapExists(t, c, "apps", "qb-private-punch") {
		t.Fatal("the ConfigMap was not written for an up punch")
	}
	cm, err := c.dyn.Resource(configMapGVR).Namespace("apps").Get(t.Context(), "qb-private-punch", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cm: %v", err)
	}
	doc, _, _ := unstructuredString(cm.Object, "data", v1alpha1.ConfigMapKey)
	if doc != `{"ip":"120.244.157.23","port":38698,"inner_port":27257,"protocol":"tcp"}` {
		t.Errorf("punch.json = %s", doc)
	}
	if c.reports.Load() != 1 {
		t.Errorf("punch_agent_reports_total = %d, want 1", c.reports.Load())
	}
}

// TestStatusEndpointLastSeenIsTheControllersClock: the agent's own notion of time is
// never trusted, and it is not even in the payload — a stale report cannot renew it.
func TestStatusEndpointLastSeenIsTheControllersClock(t *testing.T) {
	now := mustTime(t, t0)
	p := testPunch("hath", "apps", "192.168.9.71:44388", 44388)
	c, _ := newTestController(t, now, time.Minute, p)
	h := c.Handler(testToken)

	// A forged extra field must be ignored outright.
	body := `{"punches":[{"name":"hath","namespace":"apps","state":"pending","lastSeenAt":"2000-01-01T00:00:00Z","lastSeen":"2000-01-01T00:00:00Z"}]}`
	if rec := doRequest(t, h, http.MethodPost, "/punches/status", body, true); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	got := getPunch(t, c, "apps", "hath")
	if !got.Status.LastSeenAt.Time.Equal(now) {
		t.Errorf("lastSeenAt = %s, want the controller's clock %s", got.Status.LastSeenAt.Time, now)
	}
}

func TestStatusEndpointDownWritesDownDocument(t *testing.T) {
	now := mustTime(t, t0)
	p := testPunch("qb-private", "apps", "192.168.9.70:6881", 27257)
	c, client := newTestController(t, now, time.Minute, p)
	h := c.Handler(testToken)

	cm, err := BuildConfigMap(p, "120.244.157.23", 38698)
	if err != nil {
		t.Fatalf("BuildConfigMap: %v", err)
	}
	if _, err := client.Resource(configMapGVR).Namespace("apps").Create(t.Context(), cm, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed cm: %v", err)
	}

	body := `{"punches":[{"name":"qb-private","namespace":"apps","state":"down","reason":"natmap exited: exit status 1"}]}`
	if rec := doRequest(t, h, http.MethodPost, "/punches/status", body, true); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	// The object STAYS and says "down": a deletion never reaches a running pod's volume
	// (kubelet keeps the last payload), so absence cannot be the signal.
	doc := configMapDoc(t, c, "apps", "qb-private-punch")
	if doc == "" {
		t.Fatal("a down report must leave the ConfigMap in place carrying a down document")
	}
	if !saysDown(t, doc) {
		t.Errorf("document after a down report = %q, want no public tuple", doc)
	}
	got := getPunch(t, c, "apps", "qb-private")
	if got.Status.State != v1alpha1.StateDown || !strings.Contains(got.Status.Reason, "natmap exited") {
		t.Errorf("status = %q/%q, want down with the agent's reason", got.Status.State, got.Status.Reason)
	}
	// The stale public tuple is cleared, so nothing can resolve the dead mapping's port.
	if got.Status.PublicIP != "" || got.Status.PublicPort != 0 {
		t.Errorf("stale public tuple survived: %s:%d", got.Status.PublicIP, got.Status.PublicPort)
	}
}

// TestStatusEndpointPending: `pending` means "never mapped", which is said the same way
// `down` is said — in the document — so an app can be up and halted instead of blocked on
// a ConfigMap that may not exist yet.
func TestStatusEndpointPending(t *testing.T) {
	p := testPunch("hath", "apps", "192.168.9.71:44388", 44388)
	c, _ := newTestController(t, mustTime(t, t0), time.Minute, p)
	h := c.Handler(testToken)

	body := `{"punches":[{"name":"hath","namespace":"apps","state":"pending","reason":"natmap has not reported a mapping yet"}]}`
	if rec := doRequest(t, h, http.MethodPost, "/punches/status", body, true); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	doc := configMapDoc(t, c, "apps", "hath-punch")
	if doc == "" {
		t.Error("a pending punch must publish a document that says it has no mapping")
	}
	if !saysDown(t, doc) {
		t.Errorf("document for a pending punch = %q, want no public tuple", doc)
	}
	got := getPunch(t, c, "apps", "hath")
	if got.Status.State != v1alpha1.StatePending || got.Status.Reason != "natmap has not reported a mapping yet" {
		t.Errorf("status = %q/%q", got.Status.State, got.Status.Reason)
	}
}

// TestStatusEndpointBindPortConflict: the agent can be right and the declaration wrong;
// the controller's verdict wins: a port two punches claim publishes no public tuple (there
// is no admission webhook to catch it earlier).
func TestStatusEndpointBindPortConflict(t *testing.T) {
	p1 := testPunch("qb-private", "apps-qb", "192.168.9.70:6881", 27257)
	p2 := testPunch("hath", "apps-hath", "192.168.9.71:44388", 27257)
	c, _ := newTestController(t, mustTime(t, t0), time.Minute, p1, p2)
	h := c.Handler(testToken)

	body := `{"punches":[
		{"name":"qb-private","namespace":"apps-qb","state":"up","publicIP":"120.244.157.23","publicPort":38698},
		{"name":"hath","namespace":"apps-hath","state":"up","publicIP":"120.244.157.23","publicPort":38697}]}`
	if rec := doRequest(t, h, http.MethodPost, "/punches/status", body, true); rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	for _, tc := range []struct{ ns, name string }{{"apps-qb", "qb-private"}, {"apps-hath", "hath"}} {
		got := getPunch(t, c, tc.ns, tc.name)
		if got.Status.State != v1alpha1.StateDown || got.Status.Reason != v1alpha1.ReasonBindPortConflict {
			t.Errorf("%s: status = %q/%q, want down/bindPortConflict", tc.name, got.Status.State, got.Status.Reason)
		}
		// A conflicting declaration publishes no PORT — the document still exists and
		// says so, because "there is no mapping" is what the app has to act on.
		if doc := configMapDoc(t, c, tc.ns, tc.name+"-punch"); doc == "" || !saysDown(t, doc) {
			t.Errorf("%s: a conflicting punch must publish a down document, got %q", tc.name, doc)
		}
	}
}

func TestStatusEndpointRejectsBadReports(t *testing.T) {
	p := testPunch("qb-private", "apps", "192.168.9.70:6881", 27257)
	c, _ := newTestController(t, mustTime(t, t0), time.Minute, p)
	h := c.Handler(testToken)

	for _, body := range []string{
		`not json`,
		`{"punches":[{"name":"qb-private","namespace":"apps","state":"up"}]}`,                        // up without a public tuple
		`{"punches":[{"name":"qb-private","namespace":"apps","state":"exploded"}]}`,                  // not in the CRD's enum
		`{"punches":[{"name":"","namespace":"apps","state":"pending"}]}`,                             // malformed identity
		`{"punches":[{"name":"qb-private","namespace":"apps","state":"pending"},{"state":"bogus"}]}`, // one bad entry rejects the batch
	} {
		rec := doRequest(t, h, http.MethodPost, "/punches/status", body, true)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s => %d, want 400", body, rec.Code)
		}
	}
	if got := getPunch(t, c, "apps", "qb-private"); got.Status.State != "" {
		t.Errorf("a rejected batch touched the CR: state = %q", got.Status.State)
	}
}

func TestStatusEndpointUnknownPunchIsSkipped(t *testing.T) {
	p := testPunch("qb-private", "apps", "192.168.9.70:6881", 27257)
	c, _ := newTestController(t, mustTime(t, t0), time.Minute, p)
	h := c.Handler(testToken)

	body := `{"punches":[{"name":"ghost","namespace":"apps","state":"pending"},{"name":"qb-private","namespace":"apps","state":"pending"}]}`
	rec := doRequest(t, h, http.MethodPost, "/punches/status", body, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var resp statusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Accepted != 1 || resp.Skipped != 1 {
		t.Errorf("accepted/skipped = %d/%d, want 1/1", resp.Accepted, resp.Skipped)
	}
}

func TestStatusEndpointRequiresToken(t *testing.T) {
	p := testPunch("qb-private", "apps", "192.168.9.70:6881", 27257)
	c, _ := newTestController(t, mustTime(t, t0), time.Minute, p)
	h := c.Handler(testToken)

	body := `{"punches":[{"name":"qb-private","namespace":"apps","state":"up","publicIP":"1.2.3.4","publicPort":1}]}`
	if rec := doRequest(t, h, http.MethodPost, "/punches/status", body, false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", rec.Code)
	}
	if got := getPunch(t, c, "apps", "qb-private"); got.Status.State != "" {
		t.Error("an unauthenticated report reached the CR")
	}
}

func TestReconcileDeadlineAndConfigMapLifecycle(t *testing.T) {
	now := mustTime(t, t0)

	stale := testPunch("qb-private", "apps-qb", "192.168.9.70:6881", 27257)
	stale.Status = v1alpha1.PunchStatus{
		State: v1alpha1.StateUp, PublicIP: "120.244.157.23", PublicPort: 38698, InnerPort: 27257,
		LastSeenAt: metav1.NewTime(now.Add(-2 * time.Minute)),
	}
	never := testPunch("speedtest", "infra", "192.168.9.239:5201", 27500)

	fresh := testPunch("hath", "apps-hath", "192.168.9.71:44388", 44388)
	fresh.Status = v1alpha1.PunchStatus{
		State: v1alpha1.StateUp, PublicIP: "120.244.157.23", PublicPort: 38697, InnerPort: 44388,
		LastSeenAt: metav1.NewTime(now.Add(-10 * time.Second)),
	}

	c, client := newTestController(t, now, time.Minute, stale, never, fresh)

	// Seed the ConfigMaps: the stale one must go, the fresh one must stay.
	for _, p := range []*v1alpha1.Punch{stale, fresh} {
		cm, err := BuildConfigMap(p, p.Status.PublicIP, p.Status.PublicPort)
		if err != nil {
			t.Fatalf("BuildConfigMap: %v", err)
		}
		if _, err := client.Resource(configMapGVR).Namespace(p.Namespace).Create(t.Context(), cm, metav1.CreateOptions{}); err != nil {
			t.Fatalf("seed cm: %v", err)
		}
	}

	if err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	got := getPunch(t, c, "apps-qb", "qb-private")
	if got.Status.State != v1alpha1.StateDown {
		t.Errorf("a punch silent for 2m must be down, got %q", got.Status.State)
	}
	if want := "no status from the punch agent for 120s"; got.Status.Reason != want {
		t.Errorf("reason = %q, want %q", got.Status.Reason, want)
	}
	if doc := configMapDoc(t, c, "apps-qb", "qb-private-punch"); doc == "" || !saysDown(t, doc) {
		t.Errorf("a punch silent for 2m must say down in its document, got %q", doc)
	}

	if got := getPunch(t, c, "infra", "speedtest"); got.Status.State != v1alpha1.StatePending {
		t.Errorf("a never-reported punch must be pending, got %q", got.Status.State)
	}
	if doc := configMapDoc(t, c, "infra", "speedtest-punch"); doc == "" || !saysDown(t, doc) {
		t.Errorf("a never-reported punch must publish a down document, got %q", doc)
	}

	if got := getPunch(t, c, "apps-hath", "hath"); got.Status.State != v1alpha1.StateUp {
		t.Errorf("a fresh punch must stay up, got %q", got.Status.State)
	}
	if !configMapExists(t, c, "apps-hath", "hath-punch") {
		t.Error("up must keep the ConfigMap")
	}
}

// TestReconcileDoesNotRewriteTheDownDocument: the resync runs every 15s, and a punch that
// stays down would otherwise rewrite the same object forever. Content decides, so a
// second pass must write nothing.
func TestReconcileDoesNotRewriteTheDownDocument(t *testing.T) {
	now := mustTime(t, t0)
	p := testPunch("hath", "apps-hath", "192.168.9.71:44388", 44388)
	p.Status = v1alpha1.PunchStatus{State: v1alpha1.StateUp, PublicIP: "120.244.157.23",
		PublicPort: 38697, InnerPort: 44388, LastSeenAt: metav1.NewTime(now.Add(-2 * time.Minute))}
	c, _ := newTestController(t, now, time.Minute, p)

	if err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := c.writes.Load(); got != 1 {
		t.Fatalf("writes after the first reconcile = %d, want 1 (the down document)", got)
	}
	if err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := c.writes.Load(); got != 1 {
		t.Errorf("writes after an unchanged second reconcile = %d, want 1: the resync must not rewrite", got)
	}
}

// TestReconcileRecreatesMissingConfigMap: "keep it when up" includes healing a hand
// deletion, so the app's file cannot stay silently empty next to a live mapping.
func TestReconcileRecreatesMissingConfigMap(t *testing.T) {
	now := mustTime(t, t0)
	p := testPunch("hath", "apps-hath", "192.168.9.71:44388", 44388)
	p.Status = v1alpha1.PunchStatus{State: v1alpha1.StateUp, PublicIP: "120.244.157.23",
		PublicPort: 38697, InnerPort: 44388, LastSeenAt: metav1.NewTime(now.Add(-5 * time.Second))}
	c, _ := newTestController(t, now, time.Minute, p)

	if err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !configMapExists(t, c, "apps-hath", "hath-punch") {
		t.Fatal("an up punch with no ConfigMap must get one back")
	}
	// A second tick writes nothing (idempotent).
	if err := c.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile (2): %v", err)
	}
	if got := c.writes.Load(); got != 1 {
		t.Errorf("punch_configmap_writes_total = %d, want 1 (the second tick is a no-op)", got)
	}
}

func TestMetricsEndpointIsUnauthenticated(t *testing.T) {
	now := mustTime(t, t0)
	p := testPunch("qb-private", "apps", "192.168.9.70:6881", 27257)
	p.Status = v1alpha1.PunchStatus{State: v1alpha1.StateUp, PublicIP: "120.244.157.23",
		PublicPort: 38698, InnerPort: 27257, LastSeenAt: metav1.NewTime(now.Add(-5 * time.Second))}
	c, _ := newTestController(t, now, time.Minute, p)
	h := c.Handler(testToken)

	rec := doRequest(t, h, http.MethodGet, "/metrics", "", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics = %d: %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("content type = %q", ct)
	}
	for _, want := range []string{
		`punch_last_seen_timestamp_seconds{name="qb-private",namespace="apps"} `,
		`punch_state{name="qb-private",namespace="apps",state="up"} 1`,
		`punch_agent_reports_total 0`,
		`punch_configmap_writes_total 0`,
	} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("metrics missing %q:\n%s", want, rec.Body)
		}
	}
}

func TestHandlerRoutes(t *testing.T) {
	p := testPunch("qb-private", "apps", "192.168.9.70:6881", 27257)
	c, _ := newTestController(t, mustTime(t, t0), time.Minute, p)
	h := c.Handler(testToken)

	if rec := doRequest(t, h, http.MethodGet, "/punches/whatever", "", true); rec.Code != http.StatusNotFound {
		t.Errorf("unknown path = %d, want 404 (only two API endpoints are routed)", rec.Code)
	}
	if rec := doRequest(t, h, http.MethodGet, "/metrics", "", true); rec.Code != http.StatusOK {
		t.Errorf("metrics with a token = %d, want 200", rec.Code)
	}
}

var _ = unstructured.Unstructured{}
