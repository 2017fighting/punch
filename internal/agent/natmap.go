package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"sync"
	"syscall"
	"time"
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
	Stop(port int32) error
	Children() []ChildState
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
	cmd    *exec.Cmd
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
	child := &natmapChild{cmd: cmd, since: time.Now()}
	m.procs[port] = child
	m.log.Info("natmap started", "bindPort", port, "pid", cmd.Process.Pid, "argv", argv)

	go func() {
		err := cmd.Wait()
		m.mu.Lock()
		defer m.mu.Unlock()
		child.exited = true
		child.since = time.Now()
		if err != nil {
			child.err = err.Error()
			m.log.Error("natmap exited", "bindPort", port, "err", err)
			return
		}
		child.err = "exited"
		m.log.Error("natmap exited", "bindPort", port)
	}()
	return nil
}

// Stop terminates one child (its whole process group) and forgets it.
func (m *Natmap) Stop(port int32) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	child, ok := m.procs[port]
	if !ok {
		return nil
	}
	delete(m.procs, port)
	if child.cmd.Process != nil && !child.exited {
		if err := child.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			// already gone, or it never got a process: either way the map entry is gone
			m.log.Warn("natmap signal failed", "bindPort", port, "err", err)
		}
	}
	m.log.Info("natmap stopped", "bindPort", port)
	return nil
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
