package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const goodConfig = `
# punchd on the router (ADR-0044). Installed by home-ops' router files/.
cluster_url = https://punch.raenzo.com   # the Edge Gate HTTPRoute
token       = test-token
stun        = stun.m-online.net
http        = www.baidu.com
iface       = pppoe-wan
state_dir   = /var/run/punch
nft_path    = /usr/share/nftables.d/ruleset-post/punch.nft
fwmark      = 66
interval    = 15s
`

func TestParseConfig(t *testing.T) {
	cfg, err := ParseConfig([]byte(goodConfig))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	want := Config{
		ClusterURL: "https://punch.raenzo.com",
		Token:      "test-token",
		STUN:       "stun.m-online.net",
		HTTP:       "www.baidu.com",
		Iface:      "pppoe-wan",
		StateDir:   "/var/run/punch",
		NFTPath:    "/usr/share/nftables.d/ruleset-post/punch.nft",
		FWMark:     66,
		Interval:   15 * time.Second,
	}
	if cfg != want {
		t.Errorf("cfg = %+v\nwant %+v", cfg, want)
	}
	if got, want := cfg.DesiredURL(), "https://punch.raenzo.com/punches/desired"; got != want {
		t.Errorf("DesiredURL = %q, want %q", got, want)
	}
	if got, want := cfg.StatusURL(), "https://punch.raenzo.com/punches/status"; got != want {
		t.Errorf("StatusURL = %q, want %q", got, want)
	}
}

func TestParseConfigDefaults(t *testing.T) {
	cfg, err := ParseConfig([]byte(`
cluster_url = http://punch.test/
token = t
stun = stun.example
http = example.com
iface = pppoe-wan
`))
	if err != nil {
		t.Fatalf("ParseConfig: %v", err)
	}
	if cfg.StateDir != DefaultStateDir {
		t.Errorf("state_dir = %q, want %q", cfg.StateDir, DefaultStateDir)
	}
	if cfg.NFTPath != DefaultNFTPath {
		t.Errorf("nft_path = %q, want %q (fw4's auto-include)", cfg.NFTPath, DefaultNFTPath)
	}
	if cfg.Interval != 15*time.Second {
		t.Errorf("interval = %s, want 15s", cfg.Interval)
	}
	if cfg.FWMark != 0 {
		t.Errorf("fwmark = %d, want 0 (no -f)", cfg.FWMark)
	}
	if strings.HasSuffix(cfg.ClusterURL, "/") {
		t.Errorf("cluster_url keeps a trailing slash: %q", cfg.ClusterURL)
	}
}

func TestParseConfigErrors(t *testing.T) {
	base := "cluster_url = https://punch.raenzo.com\ntoken = t\nstun = s\nhttp = h\niface = i\n"
	for _, tc := range []struct{ name, body, want string }{
		{"unknown key", base + "nft_pth = /x\n", "unknown key"},
		{"missing required", "cluster_url = https://x\n", "token is required"},
		{"not key-value", base + "oops\n", "key = value"},
		{"bad url", "cluster_url = punch.raenzo.com\ntoken = t\nstun = s\nhttp = h\niface = i\n", "http(s) URL"},
		{"duplicate key", base + "token = twice\n", "duplicate key"},
		{"bad interval", base + "interval = soon\n", "positive duration"},
		{"bad fwmark", base + "fwmark = -1\n", "non-negative"},
		{"empty file", "", "required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tc.body))
			if err == nil {
				t.Fatalf("want an error mentioning %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "punch.conf")
	if err := os.WriteFile(path, []byte(goodConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Iface != "pppoe-wan" {
		t.Errorf("iface = %q", cfg.Iface)
	}
	if _, err := LoadConfig(filepath.Join(dir, "absent.conf")); err == nil {
		t.Error("a missing config file must be an error, not an empty config")
	}
}
