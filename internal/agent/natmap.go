package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"sync"
	"syscall"
	"time"
)

// The two knobs a test needs to watch a stop without sending signals or sleeping.
var (
	// stopTimeout is how long a stop waits for a child to honour SIGTERM before it SIGKILLs
	// the child's process group. natmap installs no SIGTERM handler (hev-main.c ignores only
	// SIGPIPE), so the default action — terminate — is the normal path and this only fires
	// for a child that is wedged.
	stopTimeout = 3 * time.Second
	// signalProcess is syscall.Kill.
	signalProcess = syscall.Kill
)

// ChildState is what the agent knows about one natmap child.
type ChildState struct {
	Port   int32
	Exited bool
	Err    string
	Since  time.Time
}

// NatmapRunner owns the natmap children — one per bindPort. It is an interface so the
// reconcile decisions can be tested without spawning anything.
type NatmapRunner interface {
	Start(port int32, argv []string) error
	// Stop terminates one child and waits for it to exit: the bind port is free when the
	// process is gone, not when the signal was sent.
	Stop(port int32) error
	Children() []ChildState
	// StopAll terminates every child and waits for all of them. Run calls it once on
	// ctx.Done(): the children are the agent's, so a normal `service restart` must not
	// leave them behind holding the bind ports (issue #2).
	StopAll() error
}

// Natmap is the real runner: `exec natmap ...` under this process, so procd supervises
// one thing (punchd) and punchd owns the children (ADR-0044: process liveness is the
// agent's business — natmap publishes no heartbeat of its own).
type Natmap struct {
	mu     sync.Mutex
	procs  map[int32]*natmapChild
	binary string
	log    *slog.Logger
	// NewCommand is overridable in tests; it must not start the command.
	NewCommand func(ctx context.Context, name string, args ...string) *exec.Cmd
}

type natmapChild struct {
	cmd  *exec.Cmd
	done chan struct{} // closed once cmd.Wait returned and the state is settled
	// exited, err and since are written by the reap goroutine and read under mu.
	exited bool
	err    string
	since  time.Time
}

// NewNatmap returns a runner that executes `natmap` found on PATH.
func NewNatmap(log *slog.Logger) *Natmap {
	binary, err := exec.LookPath("natmap")
	if err != nil {
		binary = "/usr/bin/natmap"
	}
	return &Natmap{
		procs:      map[int32]*natmapChild{},
		binary:     binary,
		log:        log,
		NewCommand: exec.CommandContext,
	}
}

// Start launches one child. A child already running for that port is left alone.
func (m *Natmap) Start(port int32, argv []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.procs[port]; ok {
		return nil
	}
	ctx := context.Background()
	cmd := m.NewCommand(ctx, m.binary, argv...)
	cmd.Stdout = os.Stderr // `logread | grep -i punchd` carries natmap's own output
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s %v: %w", m.binary, argv, err)
	}
	child := &natmapChild{cmd: cmd, since: time.Now(), done: make(chan struct{})}
	m.procs[port] = child
	m.log.Info("natmap started", "bindPort", port, "pid", cmd.Process.Pid, "argv", argv)
	go m.reap(port, child)
	return nil
}

// reap is the cmd.Wait goroutine: it records the exit and wakes anyone waiting in Stop.
// An exit is logged as an error only when the child was still the one the agent expected
// — a stop the agent asked for is not a crash.
func (m *Natmap) reap(port int32, child *natmapChild) {
	err := child.cmd.Wait()

	m.mu.Lock()
	child.exited = true
	child.since = time.Now()
	deliberate := m.procs[port] != child
	if !deliberate {
		switch {
		case err != nil:
			child.err = err.Error()
			m.log.Error("natmap exited", "bindPort", port, "err", err)
		default:
			child.err = "exited"
			m.log.Error("natmap exited", "bindPort", port)
		}
	}
	m.mu.Unlock()
	// After the state and the log, so a waiter woken by this close sees a settled child.
	close(child.done)
}

// Stop terminates one child (its whole process group), waits for it to exit, and forgets
// it. The wait is the point: a replacement started while the old natmap still holds the
// bind port cannot map ("Cannot assign requested address"), so "sent SIGTERM" is not
// "stopped" (issue #2).
func (m *Natmap) Stop(port int32) error {
	m.mu.Lock()
	child, ok := m.procs[port]
	delete(m.procs, port)
	m.mu.Unlock()
	if !ok {
		return nil
	}
	return m.terminate(port, child)
}

// StopAll is the shutdown path: every child is stopped, concurrently, so the whole stop
// fits in one stopTimeout instead of one per child.
func (m *Natmap) StopAll() error {
	m.mu.Lock()
	children := make(map[int32]*natmapChild, len(m.procs))
	ports := make([]int32, 0, len(m.procs))
	for port, c := range m.procs {
		children[port] = c
		ports = append(ports, port)
	}
	m.procs = map[int32]*natmapChild{}
	m.mu.Unlock()
	sort.Slice(ports, func(i, j int) bool { return ports[i] < ports[j] })

	var wg sync.WaitGroup
	errs := make([]error, len(ports))
	for i, port := range ports {
		wg.Add(1)
		go func(i int, port int32) {
			defer wg.Done()
			errs[i] = m.terminate(port, children[port])
		}(i, port)
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("stop %d natmap child(ren): %w", len(ports), err)
	}
	return nil
}

// terminate SIGTERMs the child's process group, waits for the process to go, and escalates
// to SIGKILL if it has to.
func (m *Natmap) terminate(port int32, child *natmapChild) error {
	select {
	case <-child.done:
		m.log.Info("natmap stopped", "bindPort", port)
		return nil
	default:
	}
	pid := child.cmd.Process.Pid
	if err := signalProcessGroup(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		// ESRCH: it died between the check and the signal — the wait below settles it.
		m.log.Warn("natmap signal failed", "bindPort", port, "pid", pid, "err", err)
	}
	select {
	case <-child.done:
		m.log.Info("natmap stopped", "bindPort", port)
		return nil
	case <-time.After(stopTimeout):
	}
	if err := signalProcessGroup(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		m.log.Warn("natmap SIGKILL failed", "bindPort", port, "pid", pid, "err", err)
	}
	select {
	case <-child.done:
		m.log.Warn("natmap did not stop on SIGTERM; killed", "bindPort", port, "pid", pid)
		return nil
	case <-time.After(stopTimeout):
		return fmt.Errorf("natmap for bindPort %d (pid %d) is still running after SIGKILL", port, pid)
	}
}

// signalProcessGroup signals pid's whole process group — the children are started with
// Setpgid, so the group is the pid — and falls back to the process itself when pid is not
// a group leader. ESRCH means it is already gone.
func signalProcessGroup(pid int, sig syscall.Signal) error {
	if err := signalProcess(-pid, sig); err == nil || !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return signalProcess(pid, sig)
}

// Children lists the children the runner believes it owns, sorted by port.
func (m *Natmap) Children() []ChildState {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ChildState, 0, len(m.procs))
	for port, c := range m.procs {
		out = append(out, ChildState{Port: port, Exited: c.exited, Err: c.err, Since: c.since})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

// natmapArgs is the measured command line from the research note (§3.1), with the
// production instances' `-f <fwmark>` kept when configured (-f 66 exists so the flow
// takes the right routing table; the spike added it for exactly that reason).
func natmapArgs(cfg Config, bindPort int32) []string {
	args := []string{
		"-k", "30",
		"-s", cfg.STUN,
		"-h", cfg.HTTP,
		"-b", fmt.Sprint(bindPort),
		"-4",
		"-i", cfg.Iface,
		"-e", NotifyPath(cfg.StateDir),
	}
	if cfg.FWMark > 0 {
		args = append(args, "-f", fmt.Sprint(cfg.FWMark))
	}
	return args
}

var _ NatmapRunner = (*Natmap)(nil)
