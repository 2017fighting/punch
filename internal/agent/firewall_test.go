package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const uciFirewallDefault = `config defaults
	option input 'REJECT'
	option output 'ACCEPT'
	option forward 'REJECT'
	option syn_flood '1'
	option flow_offloading '1'

config zone
	option name 'wan'
	option input 'REJECT'
`

func TestCheckAutoIncludes(t *testing.T) {
	if err := CheckAutoIncludes([]byte(uciFirewallDefault)); err != nil {
		t.Errorf("a firewall with no auto_includes option must pass (it defaults to 1): %v", err)
	}
	if err := CheckAutoIncludes([]byte(uciFirewallDefault + "\toption auto_includes '1'\n")); err != nil {
		t.Errorf("auto_includes '1' = %v, want nil", err)
	}

	bad := strings.Replace(uciFirewallDefault, "option syn_flood '1'", "option syn_flood '1'\n\toption auto_includes '0'", 1)
	err := CheckAutoIncludes([]byte(bad))
	if err == nil {
		t.Fatal("auto_includes '0' must be refused: it silently unrenders the include")
	}
	if !strings.Contains(err.Error(), "auto_includes") {
		t.Errorf("error = %v, want it to name the option", err)
	}

	// The option in a section that is not @defaults[0] is not the assertion.
	other := "config zone\n\toption auto_includes '0'\n" + uciFirewallDefault
	if err := CheckAutoIncludes([]byte(other)); err != nil {
		t.Errorf("auto_includes outside the defaults section = %v, want nil", err)
	}
}

func TestAssertInclude(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punch.nft")
	if err := AssertInclude(path); err == nil {
		t.Error("a missing include must warn, so the log says why the port went dead")
	} else if !strings.Contains(err.Error(), "firmware upgrade") {
		t.Errorf("error = %v, want it to name the cause", err)
	}
	if err := os.WriteFile(path, []byte("destroy table ip punch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AssertInclude(path); err != nil {
		t.Errorf("AssertInclude = %v, want nil", err)
	}
}
