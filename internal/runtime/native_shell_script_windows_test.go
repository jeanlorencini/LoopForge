package runtime

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func windowsTestShell(t *testing.T) string {
	t.Helper()
	path, err := findWindowsHostShell("sh")
	if err != nil {
		t.Skip(err)
	}
	return path
}

func TestWindowsHostShellPreservesLongSourceAndInput(t *testing.T) {
	temporary := filepath.Join(t.TempDir(), "临时 shell '目录")
	if err := os.MkdirAll(temporary, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMP", temporary)
	t.Setenv("TEMP", temporary)
	workspace := t.TempDir()
	script := ": \"" + strings.Repeat("x", 25000) + "\"\nIFS= read -r line\nprintf '%s|%s|%s|%s' \"$line\" \"$0\" \"$1\" \"$BOUND\""
	command := WithCommandEnvironment(CommandSpec{Provider: "script", Argv: []string{windowsTestShell(t), "-lc", script, "original-zero", "quoted \"中文\""}, Stdin: strings.NewReader("stdin '中文'\n")}, map[string]string{"BOUND": "bound 中文"})
	out, err := (&localSandbox{workspace: workspace}).Exec(context.Background(), command, nil)
	if err != nil {
		t.Fatalf("long shell: %v\n%s", err, out)
	}
	if strings.TrimSuffix(out, "\n") != "stdin '中文'|original-zero|quoted \"中文\"|bound 中文" {
		t.Fatalf("shell altered source, stdin, arguments or environment: %q", out)
	}
	entries, err := os.ReadDir(temporary)
	if err != nil || len(entries) != 0 {
		t.Fatalf("private script was not cleaned: entries=%d err=%v", len(entries), err)
	}
	if command.Argv[2] != script {
		t.Fatal("preparing command changed its original argv")
	}
}

func TestWindowsHostShellKeepsSmallScriptsInlineAndLeavesProvidersAlone(t *testing.T) {
	command := CommandSpec{Argv: []string{windowsTestShell(t), "-lc", "printf ok"}}
	got, cleanup, err := PrepareHostShellCommand(command)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if !strings.HasSuffix(got.Argv[2], command.Argv[2]) || got.Argv[1] != command.Argv[1] {
		t.Fatal("small script or flags were lost")
	}
	provider := CommandSpec{Argv: []string{"codex", "exec", strings.Repeat("x", 25000)}}
	got, cleanup, err = PrepareHostShellCommand(provider)
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	if !reflect.DeepEqual(got.Argv, provider.Argv) {
		t.Fatal("provider invocation was changed")
	}
}

func TestWindowsGitShellPreservesNativeLinkerPrecedence(t *testing.T) {
	_ = windowsTestShell(t)
	toolchain := filepath.Join(t.TempDir(), "fixture MSVC tools")
	if err := os.MkdirAll(toolchain, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(toolchain, "link.exe"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", toolchain+";"+os.Getenv("PATH"))
	t.Setenv("MSYS2_PATH_TYPE", "inherit")
	t.Setenv("ORIGINAL_PATH", "")
	if err := os.Unsetenv("ORIGINAL_PATH"); err != nil {
		t.Fatal(err)
	}
	sandbox := &localSandbox{workspace: t.TempDir()}
	for _, flags := range []string{"-lc", "-c"} {
		t.Run(flags, func(t *testing.T) {
			out, err := sandbox.Exec(t.Context(), CommandSpec{Argv: []string{"sh", flags, "command -v link.exe && command -v cygpath"}}, nil)
			if err != nil || !strings.Contains(out, "/fixture MSVC tools/link.exe") {
				t.Fatalf("shell did not pick the Windows toolchain linker and expose Git tools: %v\n%s", err, out)
			}
		})
	}
}

func TestWindowsGitShellUsesTERMOnlyAsFallback(t *testing.T) {
	shell := windowsTestShell(t)
	for _, tc := range []struct {
		name, inherited, want string
		profile, command      map[string]string
	}{
		{name: "absent", want: "dumb"},
		{name: "inherited", inherited: "fixture-host", want: "fixture-host"},
		{name: "profile", profile: map[string]string{"TERM": "fixture-profile"}, want: "fixture-profile"},
		{name: "command", inherited: "fixture-host", profile: map[string]string{"TERM": "fixture-profile"}, command: map[string]string{"TERM": "fixture-command"}, want: "fixture-command"},
		{name: "explicit-empty", command: map[string]string{"TERM": ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TERM", tc.inherited)
			if tc.inherited == "" {
				if err := os.Unsetenv("TERM"); err != nil {
					t.Fatal(err)
				}
			}
			sandbox := &localSandbox{workspace: t.TempDir(), environment: tc.profile}
			command := WithCommandEnvironment(CommandSpec{Argv: []string{shell, "-lc", `printf '<%s>' "$TERM"`}}, tc.command)
			out, err := sandbox.Exec(t.Context(), command, nil)
			if err != nil || strings.TrimSuffix(out, "\n") != "<"+tc.want+">" {
				t.Fatalf("shell TERM: got %q, want %q, error=%v", out, tc.want, err)
			}
		})
	}
}

func TestWindowsHostShellRejectsWSLAliasesAndPrefersGit(t *testing.T) {
	root := t.TempDir()
	aliases := filepath.Join(root, "WindowsApps")
	gitBin := filepath.Join(root, "Git", "bin")
	for _, directory := range []string{aliases, gitBin} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "sh.exe"), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", aliases)
	t.Setenv("ProgramFiles", filepath.Join(root, "missing"))
	t.Setenv("ProgramFiles(x86)", "")
	t.Setenv("LOCALAPPDATA", "")
	if _, err := findWindowsHostShell("sh"); err == nil {
		t.Fatal("WSL alias was selected as a host shell")
	}
	t.Setenv("ProgramFiles", root)
	command := CommandSpec{Argv: []string{"sh", "-lc", "printf ok"}}
	prepared, cleanup, err := PrepareHostShellCommand(command)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if prepared.Argv[0] != filepath.Join(gitBin, "sh.exe") || command.Argv[0] != "sh" {
		t.Fatalf("host shell did not select Git without mutating argv: %v", prepared.Argv)
	}
}
