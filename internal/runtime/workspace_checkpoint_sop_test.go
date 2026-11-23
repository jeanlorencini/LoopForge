package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCheckpointRestoresSOPState(t *testing.T) {
	if source := os.Getenv("SOP_FIXTURE_CHECKPOINT_SOURCE"); source != "" {
		archive, err := CreateWorkspaceCheckpoint(source)
		if err != nil {
			t.Fatal(err)
		}
		if err := RestoreWorkspaceCheckpoint(os.Getenv("SOP_FIXTURE_CHECKPOINT_TARGET"), archive); err != nil {
			t.Fatal(err)
		}
		return
	}

	python, args := "python3", []string{"-B", "-X", "utf8"}
	if runtime.GOOS == "windows" {
		python, args = "py", append([]string{"-3"}, args...)
	}
	if _, err := exec.LookPath(python); err != nil {
		t.Skip("SOP fixture requires Python 3")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	args = append(args, filepath.Join("..", "..", "scripts", "test-sop-reliability.py"))
	cmd := exec.Command(python, args...)
	cmd.Env = append(os.Environ(), "SOP_FIXTURE_CHECKPOINT_TOOL="+executable)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("SOP checkpoint workflow: %v\n%s", err, out)
	}
}
