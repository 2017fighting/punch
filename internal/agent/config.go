package agent

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Default paths on the router (ImmortalWrt + firewall4). NFTPath is fw4's auto-include
// post-ruleset hook — the one location that needs no UCI (research note §3.6).
const (
	DefaultStateDir = "/var/run/punch"
	DefaultNFTPath  = "/usr/share/nftables.d/ruleset-post/punch.nft"
	DefaultInterval = 15 * time.Second
)

// Config is punchd's whole configuration: read from one file, `key = value`, `#` starts
// a comment. There is no UCI and no second declaration source — the desired list lives
// in the cluster (ADR-0044: the router is a dumb actuator).
//
//	cluster_url = https://punch.raenzo.com
//	token_file  = /etc/punch/token   # 0600 file holding the bearer token (preferred)
//	token       = <inline bearer token>   # alternative; exactly one of the two
//	stun        = stun.m-online.net
//	http        = www.baidu.com
//	iface       = pppoe-wan
//	state_dir   = /var/run/punch
//	nft_path    = /usr/share/nftables.d/ruleset-post/punch.nft
//	fwmark      = 66          # optional: natmap -f <fwmark>, as the production instances use
//	interval    = 15s         # optional
type Config struct {
	ClusterURL string
	Token      string
	TokenFile  string
	STUN       string
	HTTP       string
	Iface      string
	StateDir   string
	NFTPath    string
	FWMark     int
	Interval   time.Duration
}

// ParseConfig parses the `key = value` file. Unknown keys are errors: a typo that
// silently becomes a default is exactly the kind of invisible contract ADR-0044 retires.
func ParseConfig(data []byte) (Config, error) {
	cfg := Config{
		StateDir: DefaultStateDir,
		NFTPath:  DefaultNFTPath,
		Interval: DefaultInterval,
	}
	seen := map[string]bool{}
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			return Config{}, fmt.Errorf("line %d: %q is not `key = value`", i+1, line)
		}
		key = strings.TrimSpace(strings.ToLower(key))
		val = strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' && val[len(val)-1] == '"' || val[0] == '\'' && val[len(val)-1] == '\'') {
			val = val[1 : len(val)-1]
		}
		// Allow `#` comments at the end of a value line (but not inside a token).
		if j := strings.Index(val, " #"); j >= 0 {
			val = strings.TrimSpace(val[:j])
		}
		if seen[key] {
			return Config{}, fmt.Errorf("line %d: duplicate key %q", i+1, key)
		}
		seen[key] = true
		switch key {
		case "cluster_url":
			cfg.ClusterURL = val
		case "token":
			cfg.Token = val
		case "token_file":
			cfg.TokenFile = val
		case "stun":
			cfg.STUN = val
		case "http":
			cfg.HTTP = val
		case "iface":
			cfg.Iface = val
		case "state_dir":
			cfg.StateDir = val
		case "nft_path":
			cfg.NFTPath = val
		case "fwmark":
			n, err := strconv.Atoi(val)
			if err != nil || n < 0 {
				return Config{}, fmt.Errorf("line %d: fwmark %q is not a non-negative integer", i+1, val)
			}
			cfg.FWMark = n
		case "interval":
			d, err := time.ParseDuration(val)
			if err != nil || d <= 0 {
				return Config{}, fmt.Errorf("line %d: interval %q is not a positive duration", i+1, val)
			}
			cfg.Interval = d
		default:
			return Config{}, fmt.Errorf("line %d: unknown key %q (known: cluster_url, token, token_file, stun, http, iface, state_dir, nft_path, fwmark, interval)", i+1, key)
		}
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// LoadConfig reads and parses the config file.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	cfg, err := ParseConfig(data)
	if err != nil {
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	// token_file is resolved here, not in ParseConfig: parsing stays pure, and a
	// device-local 0600 file (the router's shape, like router-backup/secrets.conf)
	// is the preferred home for the bearer token.
	if cfg.TokenFile != "" {
		raw, err := os.ReadFile(cfg.TokenFile)
		if err != nil {
			return Config{}, fmt.Errorf("read token_file %s: %w", cfg.TokenFile, err)
		}
		cfg.Token = strings.TrimSpace(string(raw))
		if cfg.Token == "" {
			return Config{}, fmt.Errorf("token_file %s is empty", cfg.TokenFile)
		}
	}
	return cfg, nil
}

func (c *Config) validate() error {
	var problems []string
	token := c.Token
	if token == "" {
		token = c.TokenFile
	}
	for _, f := range []struct{ key, val string }{
		{"cluster_url", c.ClusterURL},
		{"token or token_file", token},
		{"stun", c.STUN},
		{"http", c.HTTP},
		{"iface", c.Iface},
	} {
		if f.val == "" {
			problems = append(problems, f.key+" is required")
		}
		if f.key == "token or token_file" && c.Token != "" && c.TokenFile != "" {
			problems = append(problems, "token and token_file are both set: pick one")
		}
	}
	if c.ClusterURL != "" {
		u, err := url.Parse(c.ClusterURL)
		if err != nil || u.Scheme != "http" && u.Scheme != "https" || u.Host == "" {
			problems = append(problems, fmt.Sprintf("cluster_url %q is not an absolute http(s) URL", c.ClusterURL))
		}
	}
	if c.StateDir == "" {
		problems = append(problems, "state_dir is empty")
	}
	if c.NFTPath == "" {
		problems = append(problems, "nft_path is empty")
	}
	if len(problems) > 0 {
		return fmt.Errorf("config: %s", strings.Join(problems, "; "))
	}
	c.ClusterURL = strings.TrimRight(c.ClusterURL, "/")
	return nil
}

// DesiredURL is the endpoint the agent pulls every tick.
func (c Config) DesiredURL() string { return c.ClusterURL + "/punches/desired" }

// StatusURL is the endpoint the agent reports to.
func (c Config) StatusURL() string { return c.ClusterURL + "/punches/status" }
