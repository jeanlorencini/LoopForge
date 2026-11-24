package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"path"
	"strings"
	"sync"
	"time"

	"agentflow/internal/domain"
	"agentflow/internal/runtime"
)

// runSandboxIdleTTL releases compute resources for a shared sandbox after
// this much inactivity. Docker retains its writable layer for the next task.
const runSandboxIdleTTL = 30 * time.Minute

// runSandbox is a sandbox shared by every task of one run with the same
// sandbox definition, like the persistent per-run container of the cloud
// path. cleanup_run and daemon shutdown close it.
type runSandbox struct {
	runID    string
	mu       sync.Mutex
	sandbox  runtime.Sandbox
	active   int
	lastUsed time.Time
}

// acquireTaskSandbox returns the sandbox a task executes in and the function
// to call when the task is done. Tasks of a shared run checkout reuse one
// sandbox per definition; other tasks get a throwaway one.
func (d *Daemon) acquireTaskSandbox(ctx context.Context, task domain.RunnerTask) (runtime.Sandbox, func(), error) {
	if task.Sandbox == nil {
		return nil, nil, fmt.Errorf("frozen run sandbox definition is required")
	}
	requirements, resources := taskSandboxSpec(task)
	key, id, shared := runSandboxKey(task, requirements, resources)
	if !shared || d.cfg.DisableRunSandboxReuse {
		sandbox, err := runtime.OpenRemoteSandbox(ctx, task.ID, d.taskWorkspace(task), task.Sandbox.Profile, requirements, resources)
		if err != nil {
			return nil, nil, err
		}
		return sandbox, func() { _ = sandbox.Close(context.Background()) }, nil
	}
	d.runSandboxMu.Lock()
	if d.runSandboxes == nil {
		d.runSandboxes = map[string]*runSandbox{}
	}
	entry := d.runSandboxes[key]
	if entry == nil {
		entry = &runSandbox{runID: task.RunID}
		d.runSandboxes[key] = entry
	}
	entry.active++
	d.runSandboxMu.Unlock()
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() {
			d.runSandboxMu.Lock()
			entry.active--
			entry.lastUsed = time.Now()
			d.runSandboxMu.Unlock()
		})
	}
	entry.mu.Lock()
	defer entry.mu.Unlock()
	workspace := d.taskWorkspace(task)
	if entry.sandbox == nil {
		sandbox, err := runtime.OpenRemoteSandbox(ctx, id, workspace, task.Sandbox.Profile, requirements, resources)
		if err != nil {
			release()
			return nil, nil, err
		}
		entry.sandbox = sandbox
	} else if entry.sandbox.Workspace() != workspace {
		release()
		return nil, nil, fmt.Errorf("shared sandbox %s is bound to a different run workspace", entry.sandbox.ID())
	}
	return entry.sandbox, release, nil
}

// closeRunSandboxes closes every shared sandbox of a finished run.
func (d *Daemon) closeRunSandboxes(ctx context.Context, runID string) {
	d.closeSharedSandboxes(ctx, func(entry *runSandbox) bool { return entry.runID == runID })
}

// holdRunSandboxes keeps a panel browser action from racing idle cleanup.
func (d *Daemon) holdRunSandboxes(runID string) func() {
	d.runSandboxMu.Lock()
	var entries []*runSandbox
	for _, entry := range d.runSandboxes {
		if entry.runID == runID {
			entry.active++
			entries = append(entries, entry)
		}
	}
	d.runSandboxMu.Unlock()
	for _, entry := range entries {
		entry.mu.Lock()
		entry.mu.Unlock()
	}
	return func() {
		d.runSandboxMu.Lock()
		defer d.runSandboxMu.Unlock()
		now := time.Now()
		for _, entry := range entries {
			entry.active--
			entry.lastUsed = now
		}
	}
}

// reapIdleRunSandboxes parks Docker containers and closes other idle sandboxes.
func (d *Daemon) reapIdleRunSandboxes(ctx context.Context, idle time.Duration) {
	cutoff := time.Now().Add(-idle)
	d.runSandboxMu.Lock()
	candidates := make(map[string]*runSandbox)
	for key, entry := range d.runSandboxes {
		if entry.idleBefore(cutoff) {
			candidates[key] = entry
		}
	}
	d.runSandboxMu.Unlock()
	for key, entry := range candidates {
		if ctx.Err() != nil {
			return
		}
		d.parkIdleRunSandbox(ctx, key, entry, cutoff)
	}
}

// idleBefore is read under runSandboxMu.
func (entry *runSandbox) idleBefore(cutoff time.Time) bool {
	return entry.active == 0 && !entry.lastUsed.IsZero() && entry.lastUsed.Before(cutoff)
}

func (d *Daemon) parkIdleRunSandbox(ctx context.Context, key string, entry *runSandbox, cutoff time.Time) {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	d.runSandboxMu.Lock()
	if d.runSandboxes[key] != entry || !entry.idleBefore(cutoff) {
		d.runSandboxMu.Unlock()
		return
	}
	if sandbox, ok := entry.sandbox.(interface{ Park(context.Context) error }); ok {
		d.runSandboxMu.Unlock()
		if err := sandbox.Park(ctx); err != nil {
			log.Printf("park run %s sandbox: %v", entry.runID, err)
		}
		return
	}
	delete(d.runSandboxes, key)
	d.runSandboxMu.Unlock()
	if entry.sandbox != nil {
		_ = entry.sandbox.Close(ctx)
		entry.sandbox = nil
	}
}

// closeAllRunSandboxes runs on shutdown: a restarted daemon no longer knows
// the containers it started.
func (d *Daemon) closeAllRunSandboxes(ctx context.Context) {
	d.closeSharedSandboxes(ctx, func(*runSandbox) bool { return true })
}

// closeSharedSandboxes closes and forgets the shared sandboxes matching
// match, which is evaluated under runSandboxMu.
func (d *Daemon) closeSharedSandboxes(ctx context.Context, match func(*runSandbox) bool) {
	d.runSandboxMu.Lock()
	var entries []*runSandbox
	for key, entry := range d.runSandboxes {
		if match(entry) {
			entries = append(entries, entry)
			delete(d.runSandboxes, key)
		}
	}
	d.runSandboxMu.Unlock()
	for _, entry := range entries {
		entry.mu.Lock()
		if entry.sandbox != nil {
			_ = entry.sandbox.Close(ctx)
			entry.sandbox = nil
		}
		entry.mu.Unlock()
	}
}

// runSandboxKey identifies the shared sandbox of a task that runs in a
// shared checkout (runs/<run>/workspace). A child SOP works in its parent's
// checkout but keeps a sandbox of its own. The id doubles as the container
// name suffix, so it keeps the runner-owned "rt-" prefix that cleanup_run
// enumerates.
func runSandboxKey(task domain.RunnerTask, requirements runtime.SandboxRequirements, resources runtime.SandboxResources) (string, string, bool) {
	runID := strings.TrimSpace(task.RunID)
	if runID == "" || task.Sandbox == nil || !sharedRunWorkspace(task.Workspace) {
		return "", "", false
	}
	spec, err := json.Marshal(struct {
		Profile      domain.SandboxProfile
		Requirements runtime.SandboxRequirements
		Resources    runtime.SandboxResources
	}{task.Sandbox.Profile, requirements, resources})
	if err != nil {
		return "", "", false
	}
	sum := sha256.Sum256(spec)
	fingerprint := hex.EncodeToString(sum[:])[:12]
	return runID + "|" + fingerprint, "rt-run-" + containerSafe(runID) + "-" + fingerprint, true
}

// sharedRunWorkspace reports whether workspace is a run's shared checkout,
// runs/<run>/workspace.
func sharedRunWorkspace(workspace string) bool {
	parts := strings.Split(path.Clean(workspace), "/")
	return len(parts) == 3 && parts[0] == "runs" && parts[1] != "" && parts[1] != "." && parts[1] != ".." && parts[2] == "workspace"
}

func containerSafe(v string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '-'
	}, v)
}

// taskSandboxSpec is the sandbox a task asks for: the provider's CLI needs
// plus the Dockerfile and environment it carries.
func taskSandboxSpec(task domain.RunnerTask) (runtime.SandboxRequirements, runtime.SandboxResources) {
	resources := runtime.SandboxResources{}
	if task.Sandbox.Dockerfile != nil {
		resources.Dockerfile = *task.Sandbox.Dockerfile
	}
	if task.Sandbox.Environment != nil {
		resources.Environment = *task.Sandbox.Environment
	}
	// Tasks queued before RunnerLoginRelay was added still carry their auth mode.
	codexLoginRelay := task.Invocation != nil && task.Invocation.CredentialSource == domain.RuntimeAuthLocal &&
		task.Provider == "codex" && strings.TrimSpace(task.Sandbox.Profile.Driver) == domain.SandboxDriverDocker
	if frozen := task.Sandbox.Requirements; frozen != nil {
		requirements := runtime.SandboxRequirements{
			PrivateHome:      frozen.PrivateHome,
			BunFS:            frozen.BunFS,
			RunnerLoginRelay: frozen.RunnerLoginRelay || codexLoginRelay,
		}
		for _, state := range frozen.CLIStates {
			requirements.CLIStates = append(requirements.CLIStates, runtime.CLIStateRequirement{Directory: state.Directory, Files: append([]string(nil), state.Files...)})
		}
		return requirements, resources
	}
	requirements := runtime.ProviderRequirements(task.Provider)
	requirements.RunnerLoginRelay = codexLoginRelay
	if task.Invocation != nil && task.Invocation.CredentialSource != domain.RuntimeAuthPersonal {
		switch task.Provider {
		case "codex":
			requirements.CLIStates = []runtime.CLIStateRequirement{{Directory: ".codex"}}
		case "claude-code":
			requirements.CLIStates = []runtime.CLIStateRequirement{{Directory: ".claude", Files: []string{".claude.json"}}}
		case "codebuddy":
			requirements.CLIStates = []runtime.CLIStateRequirement{{Directory: ".codebuddy", Files: []string{".codebuddy.json"}}}
		}
	}
	return requirements, resources
}

// acquireExistingTaskSandbox takes a reference only to an existing matching
// shared environment. Status readers must never create a replacement container
// while an agent still owns the previous workspace generation.
func (d *Daemon) acquireExistingTaskSandbox(task domain.RunnerTask) (runtime.Sandbox, func()) {
	if task.Sandbox == nil || d.cfg.DisableRunSandboxReuse {
		return nil, nil
	}
	requirements, resources := taskSandboxSpec(task)
	key, _, shared := runSandboxKey(task, requirements, resources)
	if !shared {
		return nil, nil
	}
	d.runSandboxMu.Lock()
	entry := d.runSandboxes[key]
	if entry == nil {
		d.runSandboxMu.Unlock()
		return nil, nil
	}
	entry.active++
	d.runSandboxMu.Unlock()
	release := onceRelease(func() { d.runSandboxMu.Lock(); entry.active--; entry.lastUsed = time.Now(); d.runSandboxMu.Unlock() })
	entry.mu.Lock()
	sb := entry.sandbox
	entry.mu.Unlock()
	if sb == nil || sb.Workspace() != d.taskWorkspace(task) {
		release()
		return nil, nil
	}
	return sb, release
}
