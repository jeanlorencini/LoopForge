package runtime

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
)

// PrepareHostShellCommand avoids Git/MSYS silently truncating long Windows -c
// arguments. The short bootstrap sources an exact private script file, keeping
// the original shell flags, positional arguments, stdin, cwd and environment.
// It must only be used for host processes, never paths inside a container.
func PrepareHostShellCommand(command CommandSpec) (CommandSpec, func(), error) {
	return prepareHostShellCommand(command, nil)
}

func prepareHostShellCommand(command CommandSpec, defaults map[string]string) (CommandSpec, func(), error) {
	cleanup := func() {}
	if goruntime.GOOS != "windows" || len(command.Argv) < 3 {
		return command, cleanup, nil
	}
	shell := strings.TrimSuffix(strings.ToLower(filepath.Base(command.Argv[0])), ".exe")
	if shell != "sh" && shell != "bash" {
		return command, cleanup, nil
	}
	command.Argv = append([]string{}, command.Argv...)
	if !filepath.IsAbs(command.Argv[0]) {
		binary, err := findWindowsHostShell(shell)
		if err != nil {
			return command, cleanup, err
		}
		command.Argv[0] = binary
	}
	flags := command.Argv[1]
	if flags != "-c" && flags != "-lc" && flags != "-cl" {
		return command, cleanup, nil
	}
	var prelude string
	if binary, ok := windowsGitShellExecutable(command.Argv[0]); ok {
		command.Argv[0] = binary
		// Git's login profile puts Unix tools ahead of the native PATH.
		prelude = "export PATH=\"$PATH:/usr/bin:/bin:/cmd\"\n"
		if flags != "-c" {
			if !hasWindowsShellTERM(defaults, command.environment) {
				// Without TERM, Git can enable winpty aliases for a process with no terminal.
				command = WithCommandEnvironment(command, map[string]string{"TERM": "dumb"})
			}
			prelude = "export PATH=\"${ORIGINAL_PATH:+$ORIGINAL_PATH:}$PATH:/cmd\"\n"
		}
	}
	if len(prelude)+len(command.Argv[2]) < 6000 {
		command.Argv[2] = prelude + command.Argv[2]
		return command, cleanup, nil
	}
	directory, err := os.MkdirTemp("", "agentflow-shell-")
	if err != nil {
		return command, cleanup, fmt.Errorf("prepare native shell script: %w", err)
	}
	file := filepath.Join(directory, "script.sh")
	cleanup = func() {
		_ = os.Remove(file)
		_ = os.Remove(directory)
	}
	if err := os.WriteFile(file, []byte(command.Argv[2]), 0600); err != nil {
		cleanup()
		return command, func() {}, fmt.Errorf("write native shell script: %w", err)
	}
	command.Argv[2] = prelude + ". " + shellSingleQuote(filepath.ToSlash(file))
	return command, cleanup, nil
}

func hasWindowsShellTERM(overlays ...map[string]string) bool {
	if _, present := os.LookupEnv("TERM"); present {
		return true
	}
	for _, overlay := range overlays {
		for key := range overlay {
			if strings.EqualFold(key, "TERM") {
				return true
			}
		}
	}
	return false
}

// The Git launcher in bin prepends Unix tools even for non-login shells.
func windowsGitShellExecutable(binary string) (string, bool) {
	dir := filepath.Dir(binary)
	if !strings.EqualFold(filepath.Base(dir), "bin") {
		return binary, false
	}
	root := filepath.Dir(dir)
	if !strings.EqualFold(filepath.Base(root), "usr") {
		dir = filepath.Join(root, "usr", "bin")
	}
	for _, name := range []string{filepath.Base(binary), "cygpath.exe"} {
		if info, err := os.Stat(filepath.Join(dir, name)); err != nil || !info.Mode().IsRegular() {
			return binary, false
		}
	}
	return filepath.Join(dir, filepath.Base(binary)), true
}

// findWindowsHostShell prefers Git for Windows and never launches a WSL alias.
func findWindowsHostShell(shell string) (string, error) {
	var roots []string
	if git, err := exec.LookPath("git.exe"); err == nil {
		roots = append(roots, filepath.Dir(filepath.Dir(git)))
	}
	for _, base := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)")} {
		if base != "" {
			roots = append(roots, filepath.Join(base, "Git"))
		}
	}
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		roots = append(roots, filepath.Join(local, "Programs", "Git"))
	}
	var candidates []string
	for _, root := range roots {
		candidates = append(candidates, filepath.Join(root, "bin", shell+".exe"), filepath.Join(root, "usr", "bin", shell+".exe"))
	}
	for _, directory := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.IsAbs(directory) {
			candidates = append(candidates, filepath.Join(directory, shell+".exe"))
		}
	}
	for _, candidate := range candidates {
		path := strings.ToLower(filepath.ToSlash(candidate))
		if strings.Contains(path, "/windowsapps/") || strings.Contains(path, "/system32/") || strings.Contains(path, "/sysnative/") {
			continue
		}
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("Windows Runner requires Git for Windows: install Git Bash and restart the runner")
}
