package agent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// quietLog keeps the runner's own logs out of the test output.
func quietLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func sleepNatmap(ctx context.Context, name string, args ...string) *exec.Cmd {
	return exec.Command("/bin/sleep", "60")
}

// TestNatmapStopWaitsForTheProcessToExit: "sent SIGTERM" is not "stopped". The bind port
// is free only once the process is gone, which is the whole point of the stop (issue #2:
// the next generation could not bind while the old children lingered).
func TestNatmapStopWaitsForTheProcessToExit(t *testing.T) {
	m := NewNatmap(quietLog())
	m.NewCommand = sleepNatmap
	if err := m.Start(27257, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := m.procs[27257].cmd.Process.Pid

	if err := m.Stop(27257); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("the child is still there after Stop returned (pid %d): %v", pid, err)
	}
	if kids := m.Children(); len(kids) != 0 {
		t.Errorf("Children() = %+v, want none", kids)
	}
	// A second stop is a no-op, not an error: reconcile and shutdown both may ask.
	if err := m.Stop(27257); err != nil {
		t.Errorf("Stop twice: %v", err)
	}
}

// TestNatmapStopKillsAChildThatIgnoresSigterm: the escalation, for a child that is wedged.
// Without it Stop would wait forever while the bind port stays taken.
func TestNatmapStopKillsAChildThatIgnoresSigterm(t *testing.T) {
	old := stopTimeout
	stopTimeout = 200 * time.Millisecond
	t.Cleanup(func() { stopTimeout = old })

	m := NewNatmap(quietLog())
	ready := filepath.Join(t.TempDir(), "ready")
	m.NewCommand = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		// The shell ignores SIGTERM from the moment the trap is installed; the group's
		// sleep dies, the loop does not. It says so by touching $0, so the test does not
		// race the trap.
		return exec.Command("/bin/sh", "-c",
			`trap "" TERM; : >"$0"; while :; do sleep 0.05; done`, ready)
	}
	if err := m.Start(27257, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	pid := m.procs[27257].cmd.Process.Pid
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the child never installed its SIGTERM trap")
		}
		time.Sleep(5 * time.Millisecond)
	}

	start := time.Now()
	if err := m.Stop(27257); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if elapsed := time.Since(start); elapsed < stopTimeout {
		t.Errorf("Stop returned in %s, before the SIGKILL escalation (%s)", elapsed, stopTimeout)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("the child survived the escalation (pid %d): %v", pid, err)
	}
}

// TestNatmapStopAllStopsEveryChild: the shutdown path, and it has to be bounded by one
// stopTimeout — two children stopped one after the other would take two.
func TestNatmapStopAllStopsEveryChild(t *testing.T) {
	m := NewNatmap(quietLog())
	m.NewCommand = sleepNatmap
	for _, port := range []int32{27257, 27258} {
		if err := m.Start(port, nil); err != nil {
			t.Fatalf("Start(%d): %v", port, err)
		}
	}
	pids := map[int32]int{27257: m.procs[27257].cmd.Process.Pid, 27258: m.procs[27258].cmd.Process.Pid}

	if err := m.StopAll(); err != nil {
		t.Fatalf("StopAll: %v", err)
	}
	for port, pid := range pids {
		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			t.Errorf("child %d (pid %d) is still there after StopAll", port, pid)
		}
	}
	if kids := m.Children(); len(kids) != 0 {
		t.Errorf("Children() = %+v, want none", kids)
	}
	if err := m.StopAll(); err != nil {
		t.Errorf("StopAll on an empty runner: %v", err)
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestNatmapStopIsNotLoggedAsACrash: an exit the agent asked for is not the crash the
// error log is for. Otherwise every restart — the sanctioned upgrade path — pages someone.
func TestNatmapStopIsNotLoggedAsACrash(t *testing.T) {
	var buf syncBuffer
	m := NewNatmap(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	m.NewCommand = sleepNatmap
	if err := m.Start(27257, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := m.Stop(27257); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	// The reap goroutine logs before it closes the channel Stop waits on, so by now the
	// record is in the buffer.
	if logged := buf.String(); strings.Contains(logged, "natmap exited") {
		t.Errorf("a deliberate stop was logged as a crash:\n%s", logged)
	}
	// A child that dies on its own still is one.
	if err := m.Start(27258, nil); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := syscall.Kill(m.procs[27258].cmd.Process.Pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill the child: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "natmap exited") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(buf.String(), "natmap exited") {
		t.Errorf("a crash was not logged:\n%s", buf.String())
	}
}

// writeProc fakes one /proc/<pid>/cmdline under root.
func writeProc(t *testing.T, root, pid string, argv ...string) {
	t.Helper()
	dir := filepath.Join(root, pid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(argv, "\x00")
	if len(argv) > 0 {
		body += "\x00" // cmdline is NUL-terminated
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fakeKiller replaces signalProcess: it records what was signalled and treats a pid whose
// SIGTERM it has seen as gone, so the sweep's wait settles without a real process.
func fakeKiller(t *testing.T) func(pid int) []syscall.Signal {
	t.Helper()
	var mu sync.Mutex
	seen := map[int][]syscall.Signal{}
	signalProcess = func(pid int, sig syscall.Signal) error {
		mu.Lock()
		defer mu.Unlock()
		key := pid
		if key < 0 {
			key = -key
		}
		if sig == 0 && len(seen[key]) > 0 {
			return syscall.ESRCH // the mocked process is gone
		}
		seen[key] = append(seen[key], sig)
		return nil
	}
	t.Helper()
	return func(pid int) []syscall.Signal {
		mu.Lock()
		defer mu.Unlock()
		return append([]syscall.Signal(nil), seen[pid]...)
	}
}

// TestSweepLeftoversKillsOnlyOurNatmaps: the sweep's risk is the opposite of its job —
// the router also runs unrelated natmaps (their own -e script, their own -f <fwmark>),
// and the only marker that separates ours from theirs is punchd's notify path.
func TestSweepLeftoversKillsOnlyOurNatmaps(t *testing.T) {
	root := t.TempDir()
	stateDir := t.TempDir()
	notify := NotifyPath(stateDir)

	writeProc(t, root, "100", "natmap", "-k", "30", "-s", "stun.m-online.net", "-h", "www.baidu.com",
		"-b", "27257", "-4", "-i", "pppoe-wan", "-e", notify, "-f", "66") // ours
	writeProc(t, root, "101", "natmap", "-k", "30", "-b", "27800", "-e", "/usr/bin/natmap-notify")
	writeProc(t, root, "102", "/bin/sh", "-c", "natmap -e "+notify)
	writeProc(t, root, "103", "natmap", "-e", notify+".bak")
	writeProc(t, root, "104", "natmap", "-e", notify)          // ours, but skipped (this process)
	writeProc(t, root, "105", "/usr/bin/natmap", "-e", notify) // ours, spelled with a path
	writeProc(t, root, "106")                                  // a kernel thread: empty cmdline

	// The killed child's report is stale the moment it dies; the unrelated port's is not
	// ours to touch.
	writeReport(t, stateDir, 27257, "120.244.157.23", 38698)
	writeReport(t, stateDir, 27800, "120.244.157.23", 38699)

	oldRoot, oldSignal := procRoot, signalProcess
	procRoot = root
	signalled := fakeKiller(t)
	t.Cleanup(func() { procRoot, signalProcess = oldRoot, oldSignal })

	killed := sweepLeftovers(quietLog(), stateDir, map[int]bool{104: true})
	sort.Ints(killed)
	if len(killed) != 2 || killed[0] != 100 || killed[1] != 105 {
		t.Fatalf("killed = %v, want [100 105]", killed)
	}
	for _, pid := range []int{100, 105} {
		if sigs := signalled(pid); len(sigs) == 0 || sigs[0] != syscall.SIGTERM {
			t.Errorf("pid %d was signalled %v, want SIGTERM", pid, sigs)
		}
	}
	if _, err := os.Stat(ReportPath(stateDir, 27257)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the killed natmap's report survived the sweep: %v", err)
	}
	if _, err := os.Stat(ReportPath(stateDir, 27800)); err != nil {
		t.Errorf("the sweep touched a report that is not ours: %v", err)
	}
	for _, pid := range []int{101, 102, 103, 104, 106} {
		if sigs := signalled(pid); len(sigs) != 0 {
			t.Errorf("pid %d was signalled %v, want nothing", pid, sigs)
		}
	}
}

// TestSweepLeftoversSurvivesAMissingProc: punchd must start even when the sweep cannot
// look, and the tick that follows still reconciles.
func TestSweepLeftoversSurvivesAMissingProc(t *testing.T) {
	oldRoot, oldSignal := procRoot, signalProcess
	procRoot = filepath.Join(t.TempDir(), "not-a-proc")
	signalled := fakeKiller(t)
	t.Cleanup(func() { procRoot, signalProcess = oldRoot, oldSignal })

	if killed := sweepLeftovers(quietLog(), t.TempDir(), nil); len(killed) != 0 {
		t.Errorf("killed = %v, want none", killed)
	}
	if sigs := signalled(1); len(sigs) != 0 {
		t.Errorf("signalled pid 1 with %v", sigs)
	}
}
