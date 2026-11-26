package agent

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestNotifyScriptWritesTheReport(t *testing.T) {
	dir := t.TempDir()
	if err := WriteNotifyScript(dir); err != nil {
		t.Fatalf("WriteNotifyScript: %v", err)
	}
	if _, err := os.Stat(NotifyPath(dir)); err != nil {
		t.Fatalf("notify script missing: %v", err)
	}
	if fi, err := os.Stat(NotifyPath(dir)); err == nil && fi.Mode()&0o111 == 0 {
		t.Errorf("notify script is not executable: %v", fi.Mode())
	}
	if _, err := os.Stat(ReportsDir(dir)); err != nil {
		t.Fatalf("reports dir missing: %v", err)
	}

	// Exactly the arguments natmap passes on `-e` (measured in the research note §3.1).
	args := []string{"120.244.157.23", "38698", "2001::8dbc:78f4:9d17", "27257", "tcp", "10.12.15.206"}
	cmd := exec.Command("sh", append([]string{NotifyPath(dir)}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("notify script failed: %v: %s", err, out)
	}

	rep, err := ReadReport(dir, 27257)
	if err != nil {
		t.Fatalf("ReadReport: %v", err)
	}
	if rep.PublicIP != "120.244.157.23" || rep.PublicPort != 38698 || rep.InnerPort != 27257 ||
		rep.Protocol != "tcp" || rep.PrivateIP != "10.12.15.206" || rep.IP4P != "2001::8dbc:78f4:9d17" {
		t.Errorf("report = %+v", rep)
	}

	// A second change overwrites in place (the mapping moved, the port did not).
	cmd = exec.Command("sh", append([]string{NotifyPath(dir)}, "120.244.157.23", "40001", "2001::x", "27257", "tcp", "10.12.15.206")...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("second notify: %v: %s", err, out)
	}
	rep, err = ReadReport(dir, 27257)
	if err != nil {
		t.Fatalf("ReadReport (2): %v", err)
	}
	if rep.PublicPort != 40001 {
		t.Errorf("publicPort = %d, want 40001", rep.PublicPort)
	}
	if entries, _ := os.ReadDir(ReportsDir(dir)); len(entries) != 1 {
		t.Errorf("reports dir holds %d entries, want exactly 1", len(entries))
	}

	// No file at all is the ordinary first-tick state, and it must be distinguishable.
	if _, err := ReadReport(dir, 44388); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ReadReport for an unreported port = %v, want os.ErrNotExist", err)
	}
}

func TestParseReport(t *testing.T) {
	rep, err := ParseReport([]byte("120.244.157.23 36284 2001::1 27500 tcp 10.12.15.206\n"))
	if err != nil {
		t.Fatalf("ParseReport: %v", err)
	}
	if rep.PublicPort != 36284 || rep.InnerPort != 27500 {
		t.Errorf("report = %+v", rep)
	}
	for _, bad := range []string{
		"",
		"1.2.3.4 38698",
		"1.2.3.4 notaport 2001::1 27257 tcp 10.0.0.1",
		"1.2.3.4 38698 2001::1 0 tcp 10.0.0.1",
		"1.2.3.4 38698 2001::1 27257 tcp 10.0.0.1 extra",
	} {
		if _, err := ParseReport([]byte(bad)); err == nil {
			t.Errorf("ParseReport(%q) = nil error, want refusal", bad)
		}
	}
}

func TestNotifyScriptIsShellValid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notify.sh")
	if err := os.WriteFile(path, []byte(NotifyScript()), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("sh", "-n", path).CombinedOutput()
	if err != nil {
		t.Fatalf("sh -n: %v: %s", err, out)
	}
	if !strings.Contains(NotifyScript(), "Edge-triggered") {
		t.Error("the script should say why a missing file is not a death")
	}
}
