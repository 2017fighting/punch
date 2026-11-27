package agent

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The belt behind the shutdown path, for a punchd that never got to run it: procd
// SIGTERMs a stopping service, which is what Run's ctx.Done() handles, but a `kill -9` —
// or a procd term_timeout expiring on a punchd stuck in a tick — skips it, and the natmap
// children are reparented. They keep their bind port and keep refreshing the mapping they
// hold, so the punch stays `up` on the old generation while the new one cannot map
// ("Cannot assign requested address") — and nothing supervises the orphan (issue #2,
// measured 2026-11-26 on the router).
//
// Ownership is decided by the command line, not by the process name. A child of punchd
// carries punchd's own `-e <state dir>/notify.sh`; no other natmap on the device points
// its edge script there (the unrelated production instances have their own -f <fwmark>
// and their own script path). Killing every `natmap` would take those down with it — the
// one thing an init-script sweep cannot decide, and the reason this is in the binary.
//
// Both are vars so tests can point the sweep at a fixture instead of /proc.
var (
	procRoot = "/proc"
)

// sweepLeftovers kills the natmap children a previous punchd left behind and returns
// their pids. skip names pids it must not touch (this process, init). A process it cannot
// read or cannot kill is a warning, never a failure: punchd has to start either way, and
// the tick that follows still reconciles. Each killed child's report goes with it — a
// report describes a child, and the replacement only writes a new one once it has a
// mapping (the same rule the shutdown path and reconcileNatmaps follow).
func sweepLeftovers(log *slog.Logger, stateDir string, skip map[int]bool) []int {
	notifyPath := NotifyPath(stateDir)
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		log.Warn("startup sweep: cannot read the process table; a previous punchd's natmaps are not swept",
			"procRoot", procRoot, "err", err)
		return nil
	}
	var killed []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || skip[pid] {
			continue // not a process directory
		}
		argv, err := readArgv(filepath.Join(procRoot, e.Name(), "cmdline"))
		if err != nil || !ourNatmap(argv, notifyPath) {
			continue // gone between the readdir and the read, or not ours
		}
		if err := killLeftover(pid); err != nil {
			log.Warn("startup sweep: cannot kill a natmap left by a previous punchd",
				"pid", pid, "argv", argv, "err", err)
			continue
		}
		if port, ok := bindPortArg(argv); ok {
			if err := os.Remove(ReportPath(stateDir, port)); err != nil && !errors.Is(err, os.ErrNotExist) {
				log.Warn("startup sweep: cannot clear the killed natmap's report", "bindPort", port, "err", err)
			}
		}
		log.Warn("startup sweep: killed a natmap left by a previous punchd", "pid", pid, "argv", argv)
		killed = append(killed, pid)
	}
	return killed
}

// bindPortArg reads natmap's `-b <bindPort>`, the port whose report the child wrote.
func bindPortArg(argv []string) (int32, bool) {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] != "-b" {
			continue
		}
		port, err := strconv.Atoi(argv[i+1])
		if err != nil || port < 1 || port > 65535 {
			return 0, false
		}
		return int32(port), true
	}
	return 0, false
}

// readArgv reads /proc/<pid>/cmdline: NUL-separated, NUL-terminated.
func readArgv(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimRight(string(raw), "\x00")
	if trimmed == "" {
		return nil, nil // a zombie or a kernel thread
	}
	return strings.Split(trimmed, "\x00"), nil
}

// ourNatmap reports whether argv is one of punchd's own natmap children: the binary is
// natmap and its -e script is punchd's notify path.
func ourNatmap(argv []string, notifyPath string) bool {
	if len(argv) == 0 || filepath.Base(argv[0]) != "natmap" {
		return false
	}
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == "-e" && argv[i+1] == notifyPath {
			return true
		}
	}
	return false
}

// killLeftover terminates one leftover and waits for it to go: the bind port is released
// when the process is gone, not when the signal is sent.
func killLeftover(pid int) error {
	if err := signalProcessGroup(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil // already gone
		}
		return fmt.Errorf("SIGTERM: %w", err)
	}
	if waitForExit(pid, stopTimeout) {
		return nil
	}
	if err := signalProcessGroup(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("SIGKILL: %w", err)
	}
	if !waitForExit(pid, stopTimeout) {
		return fmt.Errorf("pid %d is still there after SIGKILL", pid)
	}
	return nil
}

// waitForExit polls until the pid is gone (signal 0 only tests for existence) or the
// timeout expires.
func waitForExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if err := signalProcess(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(25 * time.Millisecond)
	}
}
