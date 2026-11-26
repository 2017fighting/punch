// Package agent is the router half of the punch: it pulls the desired list from the
// cluster, keeps one natmap child per punch alive, renders the DNAT redirect into fw4's
// auto-include and reads it back, and reports what it observes.
//
// The one rule that shapes everything here: AN UNREACHABLE CLUSTER IS NOT AN EMPTY
// LIST (ADR-0044). On a failed fetch it keeps every natmap child, touches no nft table,
// and reports "cannot reach the cluster" — only an explicit absence stops a punch.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// The three states, matching the CRD's enum and api/v1alpha1's constants (a test asserts
// they cannot drift; the production binary does not import the Kubernetes types).
const (
	StatePending = "pending"
	StateUp      = "up"
	StateDown    = "down"
)

// ReasonUnreachable is what a punch reports when the desired list could not be fetched:
// the local truth is unchanged, and the cluster is told why it looks stale.
const ReasonUnreachable = "cannot reach the cluster"

// Desired is one entry of GET /punches/desired.
type Desired struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Target    string `json:"target"`
	Protocol  string `json:"protocol"`
	BindPort  int32  `json:"bindPort"`
}

type desiredResponse struct {
	Punches []Desired `json:"punches"`
}

// StatusPunch is one entry of POST /punches/status: the agent's observation, never its
// opinion of the state machine — the controller applies the deadline and the bindPort
// uniqueness rule.
type StatusPunch struct {
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	State      string `json:"state"`
	PublicIP   string `json:"publicIP"`
	PublicPort int32  `json:"publicPort"`
	InnerPort  int32  `json:"innerPort"`
	Reason     string `json:"reason"`
}

type statusBody struct {
	Punches []StatusPunch `json:"punches"`
}

// Deps are the seams: everything the agent does not own.
type Deps struct {
	HTTP   *http.Client
	Natmap NatmapRunner
	Nft    Nft
	Log    *slog.Logger
	Now    func() time.Time
	Out    io.Writer // where -dry-run prints the rendered table
}

// Agent is the reconciler.
type Agent struct {
	cfg    Config
	log    *slog.Logger
	http   *http.Client
	natmap NatmapRunner
	nft    Nft
	now    func() time.Time
	out    io.Writer
	dryRun bool

	// last good desired set: what an unreachable cluster must not be mistaken for an
	// empty one.
	desired []Desired
	// lastContent is the render as last APPLIED and VERIFIED; renderedOnce starts false
	// so the first tick after boot always re-renders (firmware upgrades overwrite
	// /usr/share).
	lastContent  string
	renderedOnce bool
	// redirectErr is the last apply/read-back failure, reported per punch.
	redirectErr error
}

// New builds an agent. It performs no I/O.
func New(cfg Config, deps Deps, dryRun bool) (*Agent, error) {
	if deps.Natmap == nil && !dryRun {
		return nil, errors.New("agent: a natmap runner is required (use -dry-run to render only)")
	}
	if deps.Nft == nil && !dryRun {
		return nil, errors.New("agent: an nft implementation is required (use -dry-run to render only)")
	}
	log := deps.Log
	if log == nil {
		log = slog.Default()
	}
	h := deps.HTTP
	if h == nil {
		h = &http.Client{Timeout: 15 * time.Second}
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	out := deps.Out
	if out == nil {
		out = os.Stdout
	}
	return &Agent{cfg: cfg, log: log, http: h, natmap: deps.Natmap, nft: deps.Nft,
		now: now, out: out, dryRun: dryRun}, nil
}

// Preflight is the two startup assertions the research note asks for (§7):
//
//  1. `firewall.@defaults[0].auto_includes` must not be 0 — that silently unrenders the
//     include while everything still looks healthy, so punchd refuses to start;
//  2. the include file may be missing because a firmware upgrade overwrote /usr/share;
//     that is a warning plus a guarantee, not a failure: the first tick always renders.
func (a *Agent) Preflight() error {
	if !a.dryRun {
		if err := WriteNotifyScript(a.cfg.StateDir); err != nil {
			return err
		}
	}
	switch data, err := os.ReadFile(firewallConfigPath); {
	case err == nil:
		if err := CheckAutoIncludes(data); err != nil {
			return err
		}
		a.log.Info("startup check: fw4 auto_includes is enabled", "path", firewallConfigPath)
	case errors.Is(err, os.ErrNotExist):
		a.log.Warn("startup check: no firewall config, skipping the auto_includes assertion", "path", firewallConfigPath)
	default:
		a.log.Warn("startup check: cannot read the firewall config", "path", firewallConfigPath, "err", err)
	}
	if err := AssertInclude(a.cfg.NFTPath); err != nil {
		a.log.Warn("startup check: include file missing, the first tick will re-render it", "path", a.cfg.NFTPath)
	} else {
		a.log.Info("startup check: include file present", "path", a.cfg.NFTPath)
	}
	return nil
}

// Run ticks immediately (so a boot self-heals without waiting) and then every interval.
func (a *Agent) Run(ctx context.Context, interval time.Duration) error {
	if err := a.Preflight(); err != nil {
		return err
	}
	if interval <= 0 {
		interval = a.cfg.Interval
	}
	if interval <= 0 {
		interval = DefaultInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	a.log.Info("reconciling", "interval", interval.String(), "cluster", a.cfg.ClusterURL,
		"nftPath", a.cfg.NFTPath, "stateDir", a.cfg.StateDir)
	for {
		if err := a.Tick(ctx); err != nil && ctx.Err() == nil {
			a.log.Error("tick failed", "err", err)
		}
		select {
		case <-ctx.Done():
			a.log.Info("stopping")
			return nil
		case <-ticker.C:
		}
	}
}

// Tick is one full pass: fetch → reconcile children → render/apply/verify → report.
func (a *Agent) Tick(ctx context.Context) error {
	desired, err := a.fetch(ctx)
	if err != nil {
		// ADR-0044: unreachable is not empty. Nothing is stopped, no table is touched.
		a.log.Error("cannot reach the cluster; keeping every running natmap and changing nothing",
			"err", err, "url", a.cfg.DesiredURL(), "running", ports(a.natmap))
		reason := ReasonUnreachable
		return a.report(ctx, a.snapshot(reason, nil, nil))
	}
	a.desired = desired

	usable, invalid := splitDesired(desired)
	startErrs := a.reconcileNatmaps(usable)

	rules := make([]Rule, 0, len(usable))
	for _, d := range usable {
		rules = append(rules, Rule{
			BindPort: d.BindPort,
			Target:   d.Target,
			Comment:  fmt.Sprintf("punch %s/%s", d.Namespace, d.Name),
		})
	}
	if err := a.applyRedirect(ctx, rules); err != nil {
		a.redirectErr = err
		a.log.Error("redirect is NOT in place", "err", err, "nftPath", a.cfg.NFTPath)
	} else {
		if a.redirectErr != nil {
			a.log.Info("redirect is in place again", "rules", len(rules))
		}
		a.redirectErr = nil
	}

	if err := a.report(ctx, a.snapshot("", invalid, startErrs)); err != nil {
		return err
	}
	return nil
}

// DryRun fetches once, prints what it would do, and touches neither nft nor natmap nor
// the cluster's status. It is the first manual run (and what tests use for the renderer).
func (a *Agent) DryRun(ctx context.Context) error {
	desired, err := a.fetch(ctx)
	if err != nil {
		return fmt.Errorf("dry-run needs the desired list (%s): %w", a.cfg.DesiredURL(), err)
	}
	a.desired = desired
	usable, invalid := splitDesired(desired)

	rules := make([]Rule, 0, len(usable))
	for _, d := range usable {
		rules = append(rules, Rule{BindPort: d.BindPort, Target: d.Target,
			Comment: fmt.Sprintf("punch %s/%s", d.Namespace, d.Name)})
	}
	content, err := Render(rules)
	if err != nil {
		return err
	}

	fmt.Fprintf(a.out, "# punchd -dry-run: %d desired punch(es) from %s\n", len(desired), a.cfg.DesiredURL())
	fmt.Fprintf(a.out, "# would be written to %s and applied with `nft -f`, then read back with `nft -j list table ip punch`\n", a.cfg.NFTPath)
	a.out.Write([]byte(content))
	for _, d := range usable {
		argv := natmapArgs(a.cfg, d.BindPort)
		fmt.Fprintf(a.out, "# would run: natmap %s\n", strings.Join(argv, " "))
	}
	for key, why := range invalid {
		fmt.Fprintf(a.out, "# invalid declaration %s: %s\n", key, why)
	}
	if len(rules) == 0 {
		fmt.Fprintf(a.out, "# no punch is desired: the table would be removed with `nft delete table ip %s`\n", TableName)
	}

	snap, err := json.MarshalIndent(a.snapshot("", invalid, nil), "", "  ")
	if err != nil {
		return err
	}
	fmt.Fprintf(a.out, "# would POST %s (not sent in -dry-run):\n%s\n", a.cfg.StatusURL(), snap)
	return nil
}

// --- fetch / report ---------------------------------------------------------------

func (a *Agent) fetch(ctx context.Context) ([]Desired, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.DesiredURL(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	req.Header.Set("Accept", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read desired body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s: %s", a.cfg.DesiredURL(), resp.Status, strings.TrimSpace(string(body)))
	}
	var doc desiredResponse
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("decode desired list: %w", err)
	}
	sort.Slice(doc.Punches, func(i, j int) bool {
		if doc.Punches[i].Namespace != doc.Punches[j].Namespace {
			return doc.Punches[i].Namespace < doc.Punches[j].Namespace
		}
		return doc.Punches[i].Name < doc.Punches[j].Name
	})
	return doc.Punches, nil
}

func (a *Agent) report(ctx context.Context, punches []StatusPunch) error {
	body, err := json.Marshal(statusBody{Punches: punches})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.StatusURL(), strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.cfg.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", a.cfg.StatusURL(), err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("POST %s: %s: %s", a.cfg.StatusURL(), resp.Status, strings.TrimSpace(string(respBody)))
	}
	states := map[string]int{}
	for _, p := range punches {
		states[p.State]++
	}
	a.log.Info("reported", "punches", len(punches), "up", states[StateUp], "pending", states[StatePending], "down", states[StateDown])
	return nil
}

// --- reconcile --------------------------------------------------------------------

func key(d Desired) string { return d.Namespace + "/" + d.Name }

// splitDesired separates declarations the agent can act on from ones it must refuse
// loudly (the CRD validates these, so this is a shape it cannot render rather than a
// thing a user is expected to write).
func splitDesired(in []Desired) (usable []Desired, invalid map[string]string) {
	invalid = map[string]string{}
	for _, d := range in {
		r := Rule{BindPort: d.BindPort, Target: d.Target}
		proto := strings.ToUpper(strings.TrimSpace(d.Protocol))
		switch {
		case r.validate() != nil:
			invalid[key(d)] = r.validate().Error()
		case proto != "" && proto != "TCP":
			invalid[key(d)] = fmt.Sprintf("protocol %q is not TCP (the carrier would map a second, unannounceable port)", d.Protocol)
		default:
			usable = append(usable, d)
		}
	}
	return usable, invalid
}

// reconcileNatmaps starts what is desired and stops what is not, and returns the ports
// that could not be started so they can be reported as down.
func (a *Agent) reconcileNatmaps(desired []Desired) map[int32]error {
	childErrs := map[int32]error{}
	running := map[int32]ChildState{}
	for _, c := range a.natmap.Children() {
		running[c.Port] = c
	}
	want := map[int32]bool{}
	for _, d := range desired {
		want[d.BindPort] = true
		if _, ok := running[d.BindPort]; ok {
			continue
		}
		argv := natmapArgs(a.cfg, d.BindPort)
		if err := a.natmap.Start(d.BindPort, argv); err != nil {
			a.log.Error("cannot start natmap", "namespace", d.Namespace, "punch", d.Name, "bindPort", d.BindPort, "err", err)
			childErrs[d.BindPort] = err
			continue
		}
		a.log.Info("started natmap for punch", "namespace", d.Namespace, "punch", d.Name, "bindPort", d.BindPort)
	}
	ports := make([]int32, 0, len(running))
	for port := range running {
		ports = append(ports, port)
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })
	for _, port := range ports {
		if want[port] {
			continue
		}
		if err := a.natmap.Stop(port); err != nil {
			a.log.Error("cannot stop natmap", "bindPort", port, "err", err)
			continue
		}
		a.log.Info("stopped natmap: no longer desired", "bindPort", port)
	}
	return childErrs
}

// applyRedirect writes the include, applies it, and reads the chain back. The order
// matters (file before nft -f): a concurrent `fw4 reload` re-reads the file.
func (a *Agent) applyRedirect(ctx context.Context, rules []Rule) error {
	content, err := Render(rules)
	if err != nil {
		return err
	}
	if a.renderedOnce && content == a.lastContent && a.redirectErr == nil {
		return nil
	}
	if a.dryRun {
		a.lastContent, a.renderedOnce = content, true
		return nil
	}
	if err := WriteInclude(a.cfg.NFTPath, content); err != nil {
		return err
	}
	if len(rules) == 0 {
		// The last punch is gone: fw4 will never delete our table (`fw4 reload` only
		// flushes inet fw4), so removing it is punchd's job (research note §0.3).
		if err := a.nft.DeleteTable(ctx); err != nil {
			a.log.Warn("nft delete table failed (it may already be gone)", "err", err)
		} else {
			a.log.Info("removed the punch table: no punch is desired")
		}
		a.lastContent, a.renderedOnce = content, true
		return nil
	}
	if err := a.nft.ApplyFile(ctx, a.cfg.NFTPath); err != nil {
		return err
	}
	dump, err := a.nft.Dump(ctx)
	if err != nil {
		return err
	}
	if err := Verify(rules, dump); err != nil {
		return err
	}
	a.lastContent, a.renderedOnce = content, true
	a.log.Info("redirect applied and verified", "rules", len(rules), "nftPath", a.cfg.NFTPath)
	return nil
}

// --- status snapshot --------------------------------------------------------------

// snapshot renders the observation per punch. It is a pure read of the children, the
// report files and the last read-back result.
func (a *Agent) snapshot(unreachableReason string, invalid map[string]string, startErrs map[int32]error) []StatusPunch {
	children := map[int32]ChildState{}
	if a.natmap != nil {
		for _, c := range a.natmap.Children() {
			children[c.Port] = c
		}
	}
	out := make([]StatusPunch, 0, len(a.desired))
	for _, d := range a.desired {
		p := StatusPunch{Name: d.Name, Namespace: d.Namespace, InnerPort: d.BindPort}
		k := key(d)
		if why, bad := invalid[k]; bad {
			p.State, p.Reason = StateDown, "invalid declaration: "+why
			out = append(out, p)
			continue
		}
		switch {
		case startErrs[d.BindPort] != nil:
			p.State, p.Reason = StateDown, "cannot start natmap: "+startErrs[d.BindPort].Error()
		case !childExists(children, d.BindPort):
			if a.dryRun {
				p.State, p.Reason = StatePending, "dry run: natmap not started"
			} else {
				p.State, p.Reason = StateDown, "natmap is not running"
			}
		case children[d.BindPort].Exited:
			why := children[d.BindPort].Err
			if why == "" {
				why = "exited"
			}
			p.State, p.Reason = StateDown, "natmap exited: "+why
		case a.redirectErr != nil:
			p.State, p.Reason = StateDown, "redirect not applied: "+a.redirectErr.Error()
		default:
			rep, err := ReadReport(a.cfg.StateDir, d.BindPort)
			switch {
			case errors.Is(err, os.ErrNotExist):
				// Edge-triggered -e: no file means "no mapping announced yet", not death.
				p.State, p.Reason = StatePending, "natmap has not reported a mapping yet"
			case err != nil:
				p.State, p.Reason = StateDown, "cannot read the natmap report: "+err.Error()
			case rep.InnerPort != d.BindPort:
				p.State = StateDown
				p.Reason = fmt.Sprintf("natmap announced inner port %d, the declaration says %d", rep.InnerPort, d.BindPort)
			default:
				p.State, p.PublicIP, p.PublicPort = StateUp, rep.PublicIP, rep.PublicPort
			}
		}
		if unreachableReason != "" && p.State != StateDown {
			// The local truth stands; the reason carries the one thing that is wrong.
			p.Reason = unreachableReason
		}
		out = append(out, p)
	}
	return out
}

func childExists(children map[int32]ChildState, port int32) bool {
	_, ok := children[port]
	return ok
}

func ports(r NatmapRunner) []int32 {
	if r == nil {
		return nil
	}
	var out []int32
	for _, c := range r.Children() {
		out = append(out, c.Port)
	}
	return out
}
