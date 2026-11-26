package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Rule is one line of the punch's redirect: "traffic that arrives on BindPort is the
// mapping for Target" (the LAN side; the carrier's public port is only ever an
// announcement — ADR-0044 fact 1).
type Rule struct {
	BindPort int32
	Target   string // host:port
	Comment  string
}

// TableName is the self-owned table. Owning it is what makes a punch change cost 60–150 ms
// instead of a whole `fw4 reload` of `inet fw4` (research note §5.2), and it is why the
// three traps below are ours to handle.
const (
	TableName = "punch"
	HookPrio  = "dstnat - 5"
)

func (r Rule) validate() error {
	if r.BindPort < 1 || r.BindPort > 65535 {
		return fmt.Errorf("bindPort %d is not in 1..65535", r.BindPort)
	}
	host, port, ok := strings.Cut(r.Target, ":")
	if !ok || host == "" || port == "" {
		return fmt.Errorf("target %q is not host:port (the CRD's pattern)", r.Target)
	}
	if strings.ContainsAny(host, " \t\"'") || strings.ContainsAny(port, " \t\"'") {
		return fmt.Errorf("target %q contains whitespace or a quote", r.Target)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("target %q has no usable port", r.Target)
	}
	return nil
}

// sanitizeComment keeps a comment from ending the nft string literal early.
func sanitizeComment(s string) string {
	r := strings.NewReplacer(`"`, "", "\\", "", "\n", " ", "\t", " ")
	return strings.TrimSpace(r.Replace(s))
}

// Render produces the include file's content.
//
// The FIRST LINE IS `destroy table ip punch`, and that is not decoration: `nft -f` treats
// `table` on an existing table as `add table` (idempotent) and then APPENDS the rules, so
// every fw4 reload would duplicate the redirect (measured in the research note §3.3).
// `destroy` returns 0 for a table that does not exist, where `delete` returns 1 and would
// fail the first boot.
func Render(rules []Rule) (string, error) {
	var b strings.Builder
	b.WriteString("destroy table ip punch\n")
	if len(rules) == 0 {
		// Nothing to redirect: the file only tears the table down, so a reload after the
		// last punch went away cannot resurrect a stale rule.
		return b.String(), nil
	}
	// Sort for a stable file: an unchanged desired set must produce an unchanged file, so
	// the "render only when it changed" rule below actually holds.
	sorted := append([]Rule(nil), rules...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].BindPort < sorted[j].BindPort })

	for _, r := range sorted {
		if err := r.validate(); err != nil {
			return "", err
		}
	}
	b.WriteString("table ip punch {\n")
	b.WriteString("\tchain prerouting {\n")
	fmt.Fprintf(&b, "\t\ttype nat hook prerouting priority %s; policy accept;\n", HookPrio)
	for _, r := range sorted {
		comment := sanitizeComment(r.Comment)
		if comment == "" {
			comment = "punch"
		}
		fmt.Fprintf(&b, "\t\ttcp dport %d dnat ip to %s comment \"%s\"\n", r.BindPort, r.Target, comment)
	}
	b.WriteString("\t}\n")
	b.WriteString("}\n")
	return b.String(), nil
}

// --- read-back ---------------------------------------------------------------------

type nftDump struct {
	Nftables []json.RawMessage `json:"nftables"`
}

type nftTable struct {
	Family string `json:"family"`
	Name   string `json:"name"`
}

type nftChain struct {
	Family string `json:"family"`
	Table  string `json:"table"`
	Name   string `json:"name"`
	Type   string `json:"type"`
	Hook   string `json:"hook"`
}

type nftRuleObj struct {
	Family  string            `json:"family"`
	Table   string            `json:"table"`
	Chain   string            `json:"chain"`
	Comment string            `json:"comment"`
	Expr    []json.RawMessage `json:"expr"`
}

type nftMatch struct {
	Op   string `json:"op"`
	Left struct {
		Payload *struct {
			Protocol string `json:"protocol"`
			Field    string `json:"field"`
		} `json:"payload"`
	} `json:"left"`
	Right any `json:"right"`
}

type nftDNAT struct {
	Family string `json:"family"`
	Addr   string `json:"addr"`
	Port   int    `json:"port"`
}

// Verify checks `nft -j list table ip punch` against what was asked for.
//
// This exists because a broken include is SILENT: `nft -f` rolls the whole batch back
// and `fw4 reload` still exits 0 (research note §3.5). The read-back is the only proof
// the redirect is there — and it also catches the duplicate-include trap, because a rule
// appears exactly once in a correct ruleset and twice after an unprotected re-include.
func Verify(want []Rule, dump []byte) error {
	var doc nftDump
	if err := json.Unmarshal(dump, &doc); err != nil {
		return fmt.Errorf("parse `nft -j list table ip punch` output: %w", err)
	}

	var haveTable, haveChain bool
	have := map[int32]string{} // bindPort -> "ip:port"
	count := map[int32]int{}
	for _, raw := range doc.Nftables {
		var el map[string]json.RawMessage
		if err := json.Unmarshal(raw, &el); err != nil {
			return fmt.Errorf("parse nft element: %w", err)
		}
		if t, ok := el["table"]; ok {
			var tbl nftTable
			if err := json.Unmarshal(t, &tbl); err != nil {
				return err
			}
			if tbl.Family == "ip" && tbl.Name == TableName {
				haveTable = true
			}
			continue
		}
		if c, ok := el["chain"]; ok {
			var ch nftChain
			if err := json.Unmarshal(c, &ch); err != nil {
				return err
			}
			if ch.Family == "ip" && ch.Table == TableName && ch.Name == "prerouting" {
				haveChain = true
				if ch.Type != "nat" || ch.Hook != "prerouting" {
					return fmt.Errorf("chain ip %s prerouting has hook %q/%q, want nat/prerouting", TableName, ch.Type, ch.Hook)
				}
			}
			continue
		}
		if r, ok := el["rule"]; ok {
			var rule nftRuleObj
			if err := json.Unmarshal(r, &rule); err != nil {
				return err
			}
			if rule.Family != "ip" || rule.Table != TableName {
				continue
			}
			port, dest := parseDNATRule(rule)
			if port == 0 || dest == "" {
				continue // not a dnat rule of ours (fw4 adds nothing here, but be strict about only our shape)
			}
			have[port] = dest
			count[port]++
			continue
		}
	}

	if len(want) == 0 {
		if haveTable {
			return fmt.Errorf("table ip %s is still present but no punch is desired", TableName)
		}
		return nil
	}
	if !haveTable || !haveChain {
		return fmt.Errorf("table ip %s / chain prerouting is absent after `nft -f` (a broken include rolls back silently and fw4 still exits 0)", TableName)
	}
	var problems []string
	for _, r := range want {
		wantDest := r.Target
		switch count[r.BindPort] {
		case 0:
			problems = append(problems, fmt.Sprintf("tcp dport %d dnat ip to %s is MISSING", r.BindPort, wantDest))
		case 1:
			if have[r.BindPort] != wantDest {
				problems = append(problems, fmt.Sprintf("tcp dport %d goes to %s, want %s", r.BindPort, have[r.BindPort], wantDest))
			}
		default:
			problems = append(problems, fmt.Sprintf("tcp dport %d appears %d times (the duplicate-include trap: `destroy table ip punch` is missing)", r.BindPort, count[r.BindPort]))
		}
	}
	wanted := make(map[int32]bool, len(want))
	for _, r := range want {
		wanted[r.BindPort] = true
	}
	for port, dest := range have {
		if !wanted[port] {
			problems = append(problems, fmt.Sprintf("unexpected redirect tcp dport %d dnat ip to %s", port, dest))
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return fmt.Errorf("read-back of table ip %s failed: %s", TableName, strings.Join(problems, "; "))
	}
	return nil
}

// parseDNATRule extracts (bindPort, "addr:port") from one rule's expression list, or
// zero values if the rule is not a `tcp dport <n> dnat ip to <addr>:<port>`.
func parseDNATRule(rule nftRuleObj) (int32, string) {
	var port int32
	var dest string
	for _, rawExpr := range rule.Expr {
		var el map[string]json.RawMessage
		if err := json.Unmarshal(rawExpr, &el); err != nil {
			continue
		}
		if m, ok := el["match"]; ok {
			var match nftMatch
			if err := json.Unmarshal(m, &match); err != nil {
				continue
			}
			if match.Left.Payload != nil && match.Left.Payload.Protocol == "tcp" && match.Left.Payload.Field == "dport" {
				switch v := match.Right.(type) {
				case float64:
					port = int32(v)
				case string:
					if n, err := strconv.Atoi(v); err == nil {
						port = int32(n)
					}
				}
			}
		}
		if d, ok := el["dnat"]; ok {
			var dnat nftDNAT
			if err := json.Unmarshal(d, &dnat); err != nil {
				continue
			}
			dest = dnat.Addr + ":" + strconv.Itoa(dnat.Port)
		}
	}
	return port, dest
}

// --- nft(8) surface ----------------------------------------------------------------

// Nft is the nft(8) surface the agent needs. It is an interface so the reconcile logic
// can be tested without root, a router, or a kernel.
type Nft interface {
	// ApplyFile runs `nft -f <path>`.
	ApplyFile(ctx context.Context, path string) error
	// Dump runs `nft -j list table ip punch` and returns the JSON.
	Dump(ctx context.Context) ([]byte, error)
	// DeleteTable runs `nft delete table ip punch` (explicit: fw4 only flushes its own
	// table, so deleting the include never removes ours — research note §0.3).
	DeleteTable(ctx context.Context) error
}

// NftExec runs the real nft binary.
type NftExec struct{ Path string }

// NewNftExec returns an Nft backed by `nft` on PATH (or /usr/sbin/nft).
func NewNftExec() *NftExec {
	if p, err := exec.LookPath("nft"); err == nil {
		return &NftExec{Path: p}
	}
	return &NftExec{Path: "/usr/sbin/nft"}
}

func (n *NftExec) ApplyFile(ctx context.Context, path string) error {
	out, err := exec.CommandContext(ctx, n.Path, "-f", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("nft -f %s: %w: %s", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (n *NftExec) Dump(ctx context.Context) ([]byte, error) {
	out, err := exec.CommandContext(ctx, n.Path, "-j", "list", "table", "ip", TableName).Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		return nil, fmt.Errorf("nft -j list table ip %s: %w: %s", TableName, err, stderr)
	}
	return out, nil
}

func (n *NftExec) DeleteTable(ctx context.Context) error {
	out, err := exec.CommandContext(ctx, n.Path, "delete", "table", "ip", TableName).CombinedOutput()
	if err != nil {
		return fmt.Errorf("nft delete table ip %s: %w: %s", TableName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// WriteInclude writes the rendered table to the fw4 auto-include path, ATOMICALLY and
// BEFORE applying it: a concurrent `fw4 reload` re-reads this file, so it must already
// hold the new rules or it would re-apply the old ones (research note §6, "先写文件，再 nft -f").
func WriteInclude(path, content string) error {
	// The include's directory is not guaranteed to exist on a default image: fw4 only
	// reads it, and the first boot of a device that has never had a punch has no
	// /usr/share/nftables.d/ruleset-post at all (observed on the first live run,
	// 2026-11-26 — the first tick failed with ENOENT). Creating it is the agent's job.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create include dir for %s: %w", path, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("rename %s -> %s: %w", tmp, path, err)
	}
	return nil
}
