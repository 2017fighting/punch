package agent

import (
	"fmt"
	"os"
	"strings"
)

// firewallConfigPath is the UCI file the auto-include assertion reads. It is a variable
// (not a config key) because it is a property of the platform, not of the deployment:
// firmware upgrades and resets always put it back here.
var firewallConfigPath = "/etc/config/firewall"

// CheckAutoIncludes implements the first startup assertion from the research note (§7.4):
// `firewall.@defaults[0].auto_includes = 0` silently stops fw4 from rendering
// /usr/share/nftables.d/ruleset-post/punch.nft, and nothing else complains — the file is
// present, the process is healthy, and the port is dead.
func CheckAutoIncludes(data []byte) error {
	inDefaults := false
	firstDefaultsSeen := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "config ") {
			fields := strings.Fields(line)
			if len(fields) >= 2 && fields[1] == "defaults" {
				if firstDefaultsSeen {
					// Only @defaults[0] exists in practice; stop at a second one rather
					// than reading a block nobody asserted about.
					return nil
				}
				firstDefaultsSeen = true
				inDefaults = true
				continue
			}
			inDefaults = false
			continue
		}
		if !inDefaults {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == "option" && fields[1] == "auto_includes" {
			val := strings.Trim(strings.Join(fields[2:], ""), `'"`)
			if val == "0" {
				return fmt.Errorf("firewall.@defaults[0].auto_includes is 0: fw4 will not render %s, so the punch redirect would exist on disk and nowhere in the ruleset", DefaultNFTPath)
			}
		}
	}
	return nil
}

// AssertInclude re-states the second assertion: a firmware upgrade overwrites /usr/share,
// which deletes the include file while leaving punchd running. The first tick rewrites it
// (`renderedOnce` starts false), and this check makes that visible in the log instead of
// being inferred from a port that stopped working.
func AssertInclude(path string) error {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%s does not exist (firmware upgrade?): the first tick re-renders it", path)
		}
		return fmt.Errorf("stat %s: %w", path, err)
	}
	return nil
}
