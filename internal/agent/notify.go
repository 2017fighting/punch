package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// notifyScript is what natmap runs on `-e`. natmap's callback is EDGE-TRIGGERED: it
// fires only when the public tuple CHANGES (`hev_stun.c`, ADR-0044 fact 4), with
//
//	{public-addr} {public-port} {ip4p} {private-port} {protocol} {private-addr}
//
// so the file's absence means "no mapping announced yet" — pending — never "down".
// The write is a rename, so a reader never sees a half line.
const notifyScript = `#!/bin/sh
# Written by punchd. Do not edit: the next tick rewrites it.
# natmap -e callback arguments:
#   $1 public address   $2 public port   $3 ip4p
#   $4 private port     $5 protocol      $6 private address
# Edge-triggered: this runs only when the public tuple changes, so a missing report
# file means "not mapped yet", never "the mapping died".
d="$(dirname "$0")/reports"
mkdir -p "$d" 2>/dev/null
if [ -z "$4" ]; then exit 0; fi
printf '%s\n' "$*" >"$d/$4.args.tmp" 2>/dev/null || exit 0
mv -f "$d/$4.args.tmp" "$d/$4.args" 2>/dev/null || exit 0
exit 0
`

// NotifyScript returns the script punchd installs for natmap's `-e`.
func NotifyScript() string { return notifyScript }

// NotifyPath is where that script lives (inside the state dir, so a reboot wipes both
// the script and the reports together — the mapping is re-announced on the next tick).
func NotifyPath(stateDir string) string { return filepath.Join(stateDir, "notify.sh") }

// ReportsDir holds one `<bindPort>.args` file per punch.
func ReportsDir(stateDir string) string { return filepath.Join(stateDir, "reports") }

// ReportPath is the file natmap's notify script writes for one bind port.
func ReportPath(stateDir string, bindPort int32) string {
	return filepath.Join(ReportsDir(stateDir), strconv.Itoa(int(bindPort))+".args")
}

// Report is one natmap announcement: the carrier's public tuple plus the inner port.
type Report struct {
	PublicIP   string
	PublicPort int32
	IP4P       string
	InnerPort  int32
	Protocol   string
	PrivateIP  string
}

// ParseReport parses the six fields natmap passes to `-e`.
func ParseReport(data []byte) (Report, error) {
	fields := strings.Fields(string(data))
	if len(fields) != 6 {
		return Report{}, fmt.Errorf("natmap report has %d fields, want 6 (%q)", len(fields), strings.TrimSpace(string(data)))
	}
	port, err := parsePort(fields[1])
	if err != nil {
		return Report{}, fmt.Errorf("public port: %w", err)
	}
	inner, err := parsePort(fields[3])
	if err != nil {
		return Report{}, fmt.Errorf("private port: %w", err)
	}
	if fields[0] == "" {
		return Report{}, fmt.Errorf("natmap report has no public address")
	}
	return Report{
		PublicIP:   fields[0],
		PublicPort: port,
		IP4P:       fields[2],
		InnerPort:  inner,
		Protocol:   strings.ToLower(fields[4]),
		PrivateIP:  fields[5],
	}, nil
}

func parsePort(s string) (int32, error) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, fmt.Errorf("%q is not a port in 1..65535", s)
	}
	return int32(n), nil
}

// ReadReport reads the report for one bind port; os.ErrNotExist means "natmap has not
// announced a mapping yet".
func ReadReport(stateDir string, bindPort int32) (Report, error) {
	raw, err := os.ReadFile(ReportPath(stateDir, bindPort))
	if err != nil {
		return Report{}, err
	}
	return ParseReport(raw)
}

// WriteNotifyScript installs the notify script and creates the reports directory.
func WriteNotifyScript(stateDir string) error {
	if err := os.MkdirAll(ReportsDir(stateDir), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", ReportsDir(stateDir), err)
	}
	if err := os.WriteFile(NotifyPath(stateDir), []byte(notifyScript), 0o755); err != nil {
		return fmt.Errorf("write %s: %w", NotifyPath(stateDir), err)
	}
	return nil
}
