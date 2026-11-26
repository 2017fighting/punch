package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRenderOnePunch pins the exact file the agent writes for one punch. The shape is
// the measured one (research note §3.6), and the first line is what stops a repeated
// include from duplicating the rule.
func TestRenderOnePunch(t *testing.T) {
	got, err := Render([]Rule{{
		BindPort: 27257,
		Target:   "192.168.9.70:6881",
		Comment:  "punch apps-qbittorrent-private/qb-private",
	}})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := "destroy table ip punch\n" +
		"table ip punch {\n" +
		"\tchain prerouting {\n" +
		"\t\ttype nat hook prerouting priority dstnat - 5; policy accept;\n" +
		"\t\ttcp dport 27257 dnat ip to 192.168.9.70:6881 comment \"punch apps-qbittorrent-private/qb-private\"\n" +
		"\t}\n" +
		"}\n"
	if got != want {
		t.Errorf("Render =\n%s\nwant\n%s", got, want)
	}
}

func TestRenderEmptyTearsTheTableDown(t *testing.T) {
	got, err := Render(nil)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != "destroy table ip punch\n" {
		t.Errorf("Render(nil) = %q, want only the destroy line", got)
	}
}

func TestRenderIsStableAndSorted(t *testing.T) {
	a, err := Render([]Rule{
		{BindPort: 44388, Target: "192.168.9.71:44388"},
		{BindPort: 27257, Target: "192.168.9.70:6881"},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	b, err := Render([]Rule{
		{BindPort: 27257, Target: "192.168.9.70:6881"},
		{BindPort: 44388, Target: "192.168.9.71:44388"},
	})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if a != b {
		t.Error("the same desired set must render byte-identically, or every tick rewrites the file")
	}
	if strings.Index(a, "27257") > strings.Index(a, "44388") {
		t.Error("rules are not sorted by bindPort")
	}
}

func TestRenderRejectsBadRules(t *testing.T) {
	for _, r := range []Rule{
		{BindPort: 0, Target: "192.168.9.70:6881"},
		{BindPort: 70000, Target: "192.168.9.70:6881"},
		{BindPort: 27257, Target: "192.168.9.70"},
		{BindPort: 27257, Target: "192.168.9.70:notaport"},
		{BindPort: 27257, Target: "192.168.9.70:6881\" oops"},
	} {
		if _, err := Render([]Rule{r}); err == nil {
			t.Errorf("Render(%+v) = nil error, want refusal", r)
		}
	}
}

// nftJSONRule is one rule element of `nft -j list table ip punch`, in nft's own shape.
func nftJSONRule(handle int, dport int, addr string, port int) string {
	return fmt.Sprintf(`{"rule": {"family": "ip", "table": "punch", "chain": "prerouting", "handle": %d,
   "expr": [{"match": {"op": "==", "left": {"payload": {"protocol": "tcp", "field": "dport"}}, "right": %d}},
            {"dnat": {"family": "ip", "addr": %q, "port": %d}}],
   "comment": "punch"}}`, handle, dport, addr, port)
}

// nftJSONDump wraps rule elements with the metainfo/table/chain headers nft emits.
func nftJSONDump(rules ...string) string {
	head := `{"nftables": [
 {"metainfo": {"version": "1.1.6", "release_name": "unknown", "json_schema_version": 1}},
 {"table": {"family": "ip", "name": "punch", "handle": 12}},
 {"chain": {"family": "ip", "table": "punch", "name": "prerouting", "handle": 1, "type": "nat", "hook": "prerouting", "prio": -105, "policy": "accept"}}`
	if len(rules) == 0 {
		return head + "]}"
	}
	return head + ",\n " + strings.Join(rules, ",\n ") + "]}"
}

var nftDumpOne = nftJSONDump(nftJSONRule(2, 27257, "192.168.9.70", 6881))

var oneRule = []Rule{{BindPort: 27257, Target: "192.168.9.70:6881"}}

func TestVerifyAcceptsTheAppliedTable(t *testing.T) {
	if err := Verify(oneRule, []byte(nftDumpOne)); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestVerifyCatchesTheThreeTraps(t *testing.T) {
	tests := []struct {
		name  string
		want  []Rule
		dump  string
		match string
	}{
		{
			name:  "the duplicate-include trap (the rule appears twice)",
			want:  oneRule,
			dump:  nftJSONDump(nftJSONRule(2, 27257, "192.168.9.70", 6881), nftJSONRule(3, 27257, "192.168.9.70", 6881)),
			match: "duplicate-include trap",
		},
		{
			name:  "a broken include rolls back silently (no table at all)",
			want:  oneRule,
			dump:  `{"nftables": [{"metainfo": {"version": "1.1.6"}}]}`,
			match: "absent",
		},
		{
			name:  "the rule is missing",
			want:  []Rule{{BindPort: 44388, Target: "192.168.9.71:44388"}},
			dump:  nftDumpOne,
			match: "MISSING",
		},
		{
			name:  "the rule points somewhere else",
			want:  []Rule{{BindPort: 27257, Target: "192.168.9.70:8080"}},
			dump:  nftDumpOne,
			match: "goes to 192.168.9.70:6881",
		},
		{
			name:  "a stale rule nobody asked for",
			want:  nil,
			dump:  nftDumpOne,
			match: "still present",
		},
		{
			name:  "not json",
			want:  oneRule,
			dump:  "Error: No such file or directory",
			match: "parse",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Verify(tc.want, []byte(tc.dump))
			if err == nil {
				t.Fatalf("Verify = nil, want an error mentioning %q", tc.match)
			}
			if !strings.Contains(err.Error(), tc.match) {
				t.Errorf("error = %v, want it to mention %q", err, tc.match)
			}
		})
	}
}

func TestVerifyEmptyDesiredNeedsNoTable(t *testing.T) {
	if err := Verify(nil, []byte(`{"nftables": [{"metainfo": {"version": "1.1.6"}}]}`)); err != nil {
		t.Errorf("Verify(nil, empty) = %v, want nil", err)
	}
}

func TestWriteIncludeIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punch.nft")
	if err := WriteInclude(path, "destroy table ip punch\n"); err != nil {
		t.Fatalf("WriteInclude: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(raw), "destroy table ip punch") {
		t.Errorf("content = %q", raw)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Error("the temporary file was left behind")
	}
}

// The include directory does not exist on a device that has never had a punch (fw4 only
// reads it), so the first tick must create it — observed live on 2026-11-26: without
// this, every tick failed with ENOENT and no redirect was ever installed.
func TestWriteIncludeCreatesMissingDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nftables.d", "ruleset-post", "punch.nft")
	if err := WriteInclude(path, "destroy table ip punch\n"); err != nil {
		t.Fatalf("WriteInclude into a missing directory: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "destroy table ip punch\n" {
		t.Fatalf("content = %q", got)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("the temp file must be renamed away, stat err = %v", err)
	}
}
