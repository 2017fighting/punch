package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/2017fighting/punch/api/v1alpha1"
)

// --- fakes ------------------------------------------------------------------------

type fakeNatmap struct {
	mu       sync.Mutex
	children map[int32]ChildState
	starts   [][]string
	stops    []int32
	startErr error
}

func newFakeNatmap() *fakeNatmap { return &fakeNatmap{children: map[int32]ChildState{}} }

func (f *fakeNatmap) Start(port int32, argv []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return f.startErr
	}
	f.starts = append(f.starts, argv)
	f.children[port] = ChildState{Port: port}
	return nil
}

func (f *fakeNatmap) Stop(port int32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops = append(f.stops, port)
	delete(f.children, port)
	return nil
}

func (f *fakeNatmap) Children() []ChildState {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ChildState, 0, len(f.children))
	for _, c := range f.children {
		out = append(out, c)
	}
	return out
}

func (f *fakeNatmap) set(port int32, c ChildState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c.Port = port
	f.children[port] = c
}

type fakeNft struct {
	applies   int
	dumps     int
	deletes   int
	applyErr  error
	dumpBytes []byte
	dumpErr   error
	applied   []string
}

func (f *fakeNft) ApplyFile(ctx context.Context, path string) error {
	f.applies++
	raw, _ := os.ReadFile(path)
	f.applied = append(f.applied, string(raw))
	return f.applyErr
}

func (f *fakeNft) Dump(ctx context.Context) ([]byte, error) {
	f.dumps++
	if f.dumpErr != nil {
		return nil, f.dumpErr
	}
	if f.dumpBytes != nil {
		return f.dumpBytes, nil
	}
	return []byte(nftJSONDump()), nil
}

func (f *fakeNft) DeleteTable(ctx context.Context) error {
	f.deletes++
	return nil
}

// fakeCluster is the controller side: it serves the desired list and records reports.
type fakeCluster struct {
	mu        sync.Mutex
	desired   []Desired
	fetchCode int
	postCode  int
	fetches   int
	posts     []statusBody
	srv       *httptest.Server
}

func newFakeCluster(t *testing.T, desired ...Desired) *fakeCluster {
	t.Helper()
	c := &fakeCluster{desired: desired}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		switch r.URL.Path {
		case "/punches/desired":
			c.fetches++
			if c.fetchCode != 0 {
				http.Error(w, "boom", c.fetchCode)
				return
			}
			_ = json.NewEncoder(w).Encode(desiredResponse{Punches: c.desired})
		case "/punches/status":
			var body statusBody
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if c.postCode != 0 {
				http.Error(w, "nope", c.postCode)
				return
			}
			c.posts = append(c.posts, body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"accepted":1}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *fakeCluster) setDesired(d ...Desired) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.desired = d
}

func (c *fakeCluster) lastPost(t *testing.T) statusBody {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.posts) == 0 {
		t.Fatal("no status report was posted")
	}
	return c.posts[len(c.posts)-1]
}

func (c *fakeCluster) postCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.posts)
}

func testConfig(t *testing.T, cluster *fakeCluster) Config {
	t.Helper()
	return Config{
		ClusterURL: cluster.srv.URL,
		Token:      "test-token",
		STUN:       "stun.m-online.net",
		HTTP:       "www.baidu.com",
		Iface:      "pppoe-wan",
		StateDir:   t.TempDir(),
		NFTPath:    filepath.Join(t.TempDir(), "punch.nft"),
		FWMark:     66,
		Interval:   15 * time.Second,
	}
}

func qbPunch() Desired {
	return Desired{Name: "qb-private", Namespace: "apps-qbittorrent-private",
		Target: "192.168.9.70:6881", Protocol: "TCP", BindPort: 27257}
}

func newTestAgent(t *testing.T, cfg Config, nat *fakeNatmap, nft *fakeNft) *Agent {
	t.Helper()
	a, err := New(cfg, Deps{Natmap: nat, Nft: nft, Log: slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: slog.LevelError}))}, false)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func writeReport(t *testing.T, stateDir string, port int32, ip string, publicPort int32) {
	t.Helper()
	if err := os.MkdirAll(ReportsDir(stateDir), 0o755); err != nil {
		t.Fatal(err)
	}
	line := fmt.Sprintf("%s %d 2001::1 %d tcp 10.12.15.206\n", ip, publicPort, port)
	if err := os.WriteFile(ReportPath(stateDir, port), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- tests ------------------------------------------------------------------------

func TestTickStartsNatmapAppliesAndReportsUp(t *testing.T) {
	cl := newFakeCluster(t, qbPunch())
	cfg := testConfig(t, cl)
	nat, nft := newFakeNatmap(), &fakeNft{dumpBytes: []byte(nftDumpOne)}
	a := newTestAgent(t, cfg, nat, nft)
	writeReport(t, cfg.StateDir, 27257, "120.244.157.23", 38698)

	if err := a.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// The exact natmap command line, including the production `-f 66` and the notify path.
	wantArgv := []string{"-k", "30", "-s", "stun.m-online.net", "-h", "www.baidu.com", "-b", "27257",
		"-4", "-i", "pppoe-wan", "-e", NotifyPath(cfg.StateDir), "-f", "66"}
	if len(nat.starts) != 1 {
		t.Fatalf("starts = %d, want 1", len(nat.starts))
	}
	if strings.Join(nat.starts[0], " ") != strings.Join(wantArgv, " ") {
		t.Errorf("argv =\n %v\nwant\n %v", nat.starts[0], wantArgv)
	}

	if nft.applies != 1 || nft.dumps != 1 {
		t.Errorf("nft applies/dumps = %d/%d, want 1/1", nft.applies, nft.dumps)
	}
	if len(nft.applied) == 1 && !strings.Contains(nft.applied[0], "tcp dport 27257 dnat ip to 192.168.9.70:6881") {
		t.Errorf("applied file:\n%s", nft.applied[0])
	}

	post := cl.lastPost(t)
	if len(post.Punches) != 1 {
		t.Fatalf("reported %d punches, want 1", len(post.Punches))
	}
	p := post.Punches[0]
	if p.State != StateUp || p.PublicIP != "120.244.157.23" || p.PublicPort != 38698 || p.InnerPort != 27257 {
		t.Errorf("report = %+v", p)
	}
	if p.Reason != "" {
		t.Errorf("reason = %q, want empty", p.Reason)
	}
}

func TestTickIsIdempotent(t *testing.T) {
	cl := newFakeCluster(t, qbPunch())
	cfg := testConfig(t, cl)
	nat, nft := newFakeNatmap(), &fakeNft{dumpBytes: []byte(nftDumpOne)}
	a := newTestAgent(t, cfg, nat, nft)
	writeReport(t, cfg.StateDir, 27257, "120.244.157.23", 38698)

	for i := 0; i < 3; i++ {
		if err := a.Tick(t.Context()); err != nil {
			t.Fatalf("Tick %d: %v", i, err)
		}
	}
	if len(nat.starts) != 1 {
		t.Errorf("starts = %d, want 1 (a child is not restarted every tick)", len(nat.starts))
	}
	if nft.applies != 1 {
		t.Errorf("nft applies = %d, want 1 (an unchanged desired set must not re-apply)", nft.applies)
	}
	if cl.postCount() != 3 {
		t.Errorf("posts = %d, want 3 (the full snapshot every tick)", cl.postCount())
	}
}

// TestTickFirstTickAlwaysRenders is the firmware-upgrade assertion: /usr/share can be
// overwritten under a running punchd, so the first pass must render even when nothing
// appears to have changed.
func TestTickFirstTickAlwaysRenders(t *testing.T) {
	cl := newFakeCluster(t, qbPunch())
	cfg := testConfig(t, cl)
	nat, nft := newFakeNatmap(), &fakeNft{dumpBytes: []byte(nftDumpOne)}
	a := newTestAgent(t, cfg, nat, nft)
	if err := a.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if nft.applies != 1 {
		t.Fatalf("the first tick did not write the include")
	}
	raw, err := os.ReadFile(cfg.NFTPath)
	if err != nil {
		t.Fatalf("include file: %v", err)
	}
	if !strings.HasPrefix(string(raw), "destroy table ip punch") {
		t.Errorf("include file = %q", raw)
	}
}

// TestTickFetchErrorKeepsEverything is ADR-0044's central rule: an unreachable cluster is
// not an empty list. Nothing is stopped, no table is touched, and the reason says why.
func TestTickFetchErrorKeepsEverything(t *testing.T) {
	cl := newFakeCluster(t, qbPunch())
	cfg := testConfig(t, cl)
	nat, nft := newFakeNatmap(), &fakeNft{dumpBytes: []byte(nftDumpOne)}
	a := newTestAgent(t, cfg, nat, nft)
	writeReport(t, cfg.StateDir, 27257, "120.244.157.23", 38698)

	if err := a.Tick(t.Context()); err != nil {
		t.Fatalf("first Tick: %v", err)
	}
	cl.mu.Lock()
	cl.fetchCode = http.StatusInternalServerError
	cl.mu.Unlock()

	if err := a.Tick(t.Context()); err != nil {
		t.Fatalf("second Tick returned %v; a fetch failure is reported, not fatal", err)
	}
	if len(nat.stops) != 0 {
		t.Errorf("stops = %v, want none: an unreachable cluster must not stop a punch", nat.stops)
	}
	if len(nat.starts) != 1 {
		t.Errorf("starts = %d, want 1", len(nat.starts))
	}
	if nft.applies != 1 || nft.deletes != 0 {
		t.Errorf("nft applies/deletes = %d/%d, want 1/0: no table is touched on a failed fetch", nft.applies, nft.deletes)
	}
	post := cl.lastPost(t)
	if len(post.Punches) != 1 || post.Punches[0].State != StateUp {
		t.Fatalf("report = %+v, want the punch still up", post.Punches)
	}
	if post.Punches[0].Reason != ReasonUnreachable {
		t.Errorf("reason = %q, want %q", post.Punches[0].Reason, ReasonUnreachable)
	}
}

func TestTickStopsWhatIsNoLongerDesiredAndDeletesTheTable(t *testing.T) {
	cl := newFakeCluster(t, qbPunch())
	cfg := testConfig(t, cl)
	nat, nft := newFakeNatmap(), &fakeNft{dumpBytes: []byte(nftDumpOne)}
	a := newTestAgent(t, cfg, nat, nft)
	if err := a.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	cl.setDesired() // the CR was deleted
	nft.dumpBytes = nil
	if err := a.Tick(t.Context()); err != nil {
		t.Fatalf("Tick (empty): %v", err)
	}
	if len(nat.stops) != 1 || nat.stops[0] != 27257 {
		t.Errorf("stops = %v, want [27257]", nat.stops)
	}
	if nft.deletes != 1 {
		t.Errorf("deletes = %d, want 1: fw4 never deletes table ip punch, punchd must", nft.deletes)
	}
	raw, err := os.ReadFile(cfg.NFTPath)
	if err != nil {
		t.Fatalf("include file: %v", err)
	}
	if string(raw) != "destroy table ip punch\n" {
		t.Errorf("include file = %q, want only the destroy line", raw)
	}
	if post := cl.lastPost(t); len(post.Punches) != 0 {
		t.Errorf("report = %+v, want an empty snapshot", post.Punches)
	}
}

func TestTickReportsPendingBeforeAnyMapping(t *testing.T) {
	cl := newFakeCluster(t, qbPunch())
	cfg := testConfig(t, cl)
	a := newTestAgent(t, cfg, newFakeNatmap(), &fakeNft{dumpBytes: []byte(nftDumpOne)})

	if err := a.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	p := cl.lastPost(t).Punches[0]
	if p.State != StatePending {
		t.Errorf("state = %q, want pending (no report file is not a death: -e is edge-triggered)", p.State)
	}
	if !strings.Contains(p.Reason, "has not reported") {
		t.Errorf("reason = %q", p.Reason)
	}
}

// TestTickRestartsNatmapAfterExit: a natmap that exits is replaced by the agent, because
// nothing else ever would — natmap publishes no heartbeat, and the runner kept the dead
// entry in its map, so the punch reported "natmap exited" until punchd itself restarted
// (measured 2026-11-26: a killed natmap left the punch down for minutes until punchd was
// restarted by hand).
func TestTickRestartsNatmapAfterExit(t *testing.T) {
	cl := newFakeCluster(t, qbPunch())
	cfg := testConfig(t, cl)
	nat := newFakeNatmap()
	a := newTestAgent(t, cfg, nat, &fakeNft{dumpBytes: []byte(nftDumpOne)})

	if err := a.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(nat.starts) != 1 {
		t.Fatalf("starts = %d, want the one child", len(nat.starts))
	}
	writeReport(t, cfg.StateDir, 27257, "120.244.157.23", 38698)
	nat.set(27257, ChildState{Exited: true, Err: "exit status 1"})

	if err := a.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(nat.stops) != 1 || nat.stops[0] != 27257 {
		t.Errorf("stops = %v, want the dead child cleared before it is replaced", nat.stops)
	}
	if len(nat.starts) != 2 {
		t.Fatalf("starts = %d, want the dead child replaced", len(nat.starts))
	}
	// The dead child's report was cleared, so the punch is "nothing announced yet" rather
	// than re-announcing the dead child's public port.
	if p := cl.lastPost(t).Punches[0]; p.State != StatePending {
		t.Errorf("report = %+v, want pending until the replacement reports", p)
	}

	// The replacement announces its own mapping — possibly a different public port, which
	// is exactly why the old report could not be kept.
	writeReport(t, cfg.StateDir, 27257, "120.244.157.23", 40000)
	if err := a.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	p := cl.lastPost(t).Punches[0]
	if p.State != StateUp || p.PublicPort != 40000 {
		t.Errorf("report = %+v, want up on the replacement's port", p)
	}
}

// TestRestartBackoffSpacesOutACrashLoop: a natmap that dies on start must not be restarted
// on every tick. The wait doubles from RestartInitialDelay, so a natmap that is simply
// broken is retried every RestartMaxDelay instead of 5 760 times a day.
func TestRestartBackoffSpacesOutACrashLoop(t *testing.T) {
	cl := newFakeCluster(t, qbPunch())
	cfg := testConfig(t, cl)
	nat := newFakeNatmap()
	a := newTestAgent(t, cfg, nat, &fakeNft{dumpBytes: []byte(nftDumpOne)})
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }

	if err := a.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(nat.starts) != 1 {
		t.Fatalf("starts = %d, want 1", len(nat.starts))
	}

	// Each death at a controlled moment: the first is replaced at once (the child ran for
	// an unknown time), and after that the waits are 15s, then 30s.
	steps := []struct {
		advance    time.Duration
		wantStarts int
		wantReason string
	}{
		{0, 2, ""},
		{5 * time.Second, 2, "retrying in"},
		{10 * time.Second, 3, ""},
	}
	for i, st := range steps {
		now = now.Add(st.advance)
		nat.set(27257, ChildState{Exited: true, Err: "exit status 1"})
		if err := a.Tick(t.Context()); err != nil {
			t.Fatalf("step %d: Tick: %v", i, err)
		}
		if got := len(nat.starts); got != st.wantStarts {
			t.Fatalf("step %d: starts = %d, want %d (backoff)", i, got, st.wantStarts)
		}
		if st.wantReason != "" {
			if p := cl.lastPost(t).Punches[0]; !strings.Contains(p.Reason, st.wantReason) {
				t.Errorf("step %d: reason = %q, want it to say %q", i, p.Reason, st.wantReason)
			}
		}
	}
}

func TestTickReportsDownWhenReadBackFails(t *testing.T) {
	cl := newFakeCluster(t, qbPunch())
	cfg := testConfig(t, cl)
	// A dump with no rule: the `nft -f` "succeeded" but the redirect is not there — the
	// silent failure the read-back exists for.
	a := newTestAgent(t, cfg, newFakeNatmap(), &fakeNft{dumpBytes: []byte(nftJSONDump())})
	writeReport(t, cfg.StateDir, 27257, "120.244.157.23", 38698)

	if err := a.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	p := cl.lastPost(t).Punches[0]
	if p.State != StateDown || !strings.Contains(p.Reason, "redirect not applied") {
		t.Errorf("report = %+v, want down with the read-back failure", p)
	}
}

func TestTickReportsDownWhenNatmapCannotStart(t *testing.T) {
	cl := newFakeCluster(t, qbPunch())
	cfg := testConfig(t, cl)
	nat := newFakeNatmap()
	nat.startErr = errors.New("exec: \"natmap\": executable file not found in $PATH")
	a := newTestAgent(t, cfg, nat, &fakeNft{dumpBytes: []byte(nftDumpOne)})

	if err := a.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	p := cl.lastPost(t).Punches[0]
	if p.State != StateDown || !strings.Contains(p.Reason, "cannot start natmap") {
		t.Errorf("report = %+v, want down with the start failure", p)
	}
}

func TestTickRefusesAnInvalidDeclaration(t *testing.T) {
	bad := Desired{Name: "broken", Namespace: "apps", Target: "not-a-host-port", Protocol: "UDP", BindPort: 27258}
	cl := newFakeCluster(t, bad)
	cfg := testConfig(t, cl)
	nat, nft := newFakeNatmap(), &fakeNft{dumpBytes: []byte(nftJSONDump())}
	a := newTestAgent(t, cfg, nat, nft)

	if err := a.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if len(nat.starts) != 0 {
		t.Errorf("started a natmap for an unrenderable declaration: %v", nat.starts)
	}
	p := cl.lastPost(t).Punches[0]
	if p.State != StateDown || !strings.Contains(p.Reason, "invalid declaration") {
		t.Errorf("report = %+v, want down/invalid declaration", p)
	}
	// The table is still cleaned up: nothing is correctly redirectable.
	if nft.deletes != 1 {
		t.Errorf("deletes = %d, want 1", nft.deletes)
	}
}

func TestTickReportsAnInnerPortMismatch(t *testing.T) {
	cl := newFakeCluster(t, qbPunch())
	cfg := testConfig(t, cl)
	a := newTestAgent(t, cfg, newFakeNatmap(), &fakeNft{dumpBytes: []byte(nftDumpOne)})
	// natmap announced 27500 while the declaration says 27257: the mapping is not the
	// punch's, so publishing its public port would announce the wrong hole.
	if err := os.MkdirAll(ReportsDir(cfg.StateDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ReportPath(cfg.StateDir, 27257),
		[]byte("120.244.157.23 38698 2001::1 27500 tcp 10.12.15.206\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := a.Tick(t.Context()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	p := cl.lastPost(t).Punches[0]
	if p.State != StateDown || !strings.Contains(p.Reason, "inner port") {
		t.Errorf("report = %+v, want down/inner port mismatch", p)
	}
}

func TestDryRunTouchesNothing(t *testing.T) {
	cl := newFakeCluster(t, qbPunch())
	cfg := testConfig(t, cl)
	var out strings.Builder
	nat, nft := newFakeNatmap(), &fakeNft{dumpBytes: []byte(nftDumpOne)}
	a, err := New(cfg, Deps{Natmap: nat, Nft: nft, Out: &out,
		Log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))}, true)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.DryRun(t.Context()); err != nil {
		t.Fatalf("DryRun: %v", err)
	}

	if len(nat.starts) != 0 || len(nat.stops) != 0 {
		t.Errorf("dry-run touched natmap: starts=%v stops=%v", nat.starts, nat.stops)
	}
	if nft.applies != 0 || nft.dumps != 0 || nft.deletes != 0 {
		t.Errorf("dry-run touched nft: %+v", nft)
	}
	if _, err := os.Stat(cfg.NFTPath); !os.IsNotExist(err) {
		t.Error("dry-run wrote the include file")
	}
	if cl.postCount() != 0 {
		t.Error("dry-run posted a status report")
	}
	printed := out.String()
	for _, want := range []string{
		"destroy table ip punch",
		"tcp dport 27257 dnat ip to 192.168.9.70:6881",
		"# would run: natmap -k 30 -s stun.m-online.net",
		"# would POST",
		"dry run: natmap not started",
	} {
		if !strings.Contains(printed, want) {
			t.Errorf("dry-run output missing %q:\n%s", want, printed)
		}
	}
}

func TestDryRunFailsLoudlyWithoutTheCluster(t *testing.T) {
	cl := newFakeCluster(t, qbPunch())
	cl.fetchCode = http.StatusForbidden
	cfg := testConfig(t, cl)
	a, err := New(cfg, Deps{}, true)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := a.DryRun(t.Context()); err == nil {
		t.Fatal("a dry run that cannot fetch must fail, not print an empty table")
	}
}

func TestPreflightRefusesAutoIncludesZero(t *testing.T) {
	dir := t.TempDir()
	fw := filepath.Join(dir, "firewall")
	if err := os.WriteFile(fw, []byte("config defaults\n\toption auto_includes '0'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := firewallConfigPath
	firewallConfigPath = fw
	t.Cleanup(func() { firewallConfigPath = old })

	cl := newFakeCluster(t)
	a := newTestAgent(t, testConfig(t, cl), newFakeNatmap(), &fakeNft{})
	if err := a.Preflight(); err == nil {
		t.Fatal("Preflight must refuse to run when fw4 will not render the include")
	}
}

func TestPreflightInstallsTheNotifyScript(t *testing.T) {
	dir := t.TempDir()
	fw := filepath.Join(dir, "firewall")
	if err := os.WriteFile(fw, []byte(uciFirewallDefault), 0o644); err != nil {
		t.Fatal(err)
	}
	old := firewallConfigPath
	firewallConfigPath = fw
	t.Cleanup(func() { firewallConfigPath = old })

	cl := newFakeCluster(t)
	cfg := testConfig(t, cl)
	a := newTestAgent(t, cfg, newFakeNatmap(), &fakeNft{})
	if err := a.Preflight(); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if _, err := os.Stat(NotifyPath(cfg.StateDir)); err != nil {
		t.Errorf("notify script not installed: %v", err)
	}
}

func TestNatmapArgsWithoutFwmark(t *testing.T) {
	cfg := Config{STUN: "s", HTTP: "h", Iface: "pppoe-wan", StateDir: "/run/punch"}
	got := strings.Join(natmapArgs(cfg, 27500), " ")
	want := "-k 30 -s s -h h -b 27500 -4 -i pppoe-wan -e /run/punch/notify.sh"
	if got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}
}

// TestStateConstantsAreTheContract keeps the agent's strings glued to the CRD's enum
// without making the router binary import Kubernetes: the test may, the binary must not.
func TestStateConstantsAreTheContract(t *testing.T) {
	for _, tc := range [][2]string{
		{StatePending, v1alpha1.StatePending},
		{StateUp, v1alpha1.StateUp},
		{StateDown, v1alpha1.StateDown},
	} {
		if tc[0] != tc[1] {
			t.Errorf("agent state %q != CRD state %q", tc[0], tc[1])
		}
	}
}
