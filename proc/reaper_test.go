package proc_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/colinrgodsey/wackyproc/proc"
)

// procPPID reads the PPID of a pid from /proc/<pid>/stat (field 4 after comm).
func procPPID(t *testing.T, pid int) (int, error) {
	t.Helper()
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	// stat format: pid (comm) state ppid ... comm may contain spaces/parens; the ppid
	// is the field after the closing paren.
	s := string(data)
	close := strings.LastIndex(s, ")")
	if close < 0 {
		return 0, fmt.Errorf("no closing paren in stat: %q", s)
	}
	fields := strings.Fields(s[close+1:])
	if len(fields) < 2 {
		return 0, fmt.Errorf("stat too short after comm: %q", s)
	}
	return strconv.Atoi(fields[1])
}

// buildWackyproc builds the real binary once for spawn-in-test.
func buildWackyproc(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "wackyproc-bin-*")
	if err != nil {
		t.Fatalf("temp: %v", err)
	}
	out := filepath.Join(dir, "wackyproc")
	cmd := exec.Command("go", "build", "-o", out, "..")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build wackyproc: %v\n%s", err, b)
	}
	return out
}

// TestSupervisorAdoptsAndReapsOrphanedGrandchild is the defunct-process regression:
// a tool that backgrounds a child and exits immediately orphans that child. With the
// subreaper fix the SUPERVISOR adopts it (its PPID becomes the supervisor), and the
// supervisor's linger-and-reap drains it - it is never handed to init as a defunct
// accumulation. Without the fix the orphan reparents to whatever subreaper/init the
// host has (here systemd --user), which is exactly the isolated-container accumulation
// when that init does not reap.
func TestSupervisorAdoptsAndReapsOrphanedGrandchild(t *testing.T) {
	if !proc.PidfdSupportedOnThisPlatform() {
		t.Skip("subreaper behavior is Linux-specific")
	}
	bin := buildWackyproc(t)

	cwd := setupTestEnv(t)
	createExecutable(t, cwd, "orphan-maker", "sleep 0.8 & exit 0")

	cmd := exec.Command(bin, "run", "orphan-maker")
	cmd.Dir = cwd
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("run start: %v", err)
	}
	defer func() { _ = cmd.Process.Kill() }()

	// Find the supervisor pid from the record, polling until the record exists (the
	// run command creates it asynchronously).
	var spid int
	for i := 0; i < 200 && spid == 0; i++ {
		records, err := os.ReadDir(filepath.Join(cwd, proc.ProcDirName))
		if err == nil {
			for _, r := range records {
				data, err := os.ReadFile(filepath.Join(cwd, proc.ProcDirName, r.Name(), proc.SupervisorPIDFileName))
				if err == nil {
					spid, _ = strconv.Atoi(strings.TrimSpace(string(data)))
					break
				}
			}
		}
		if spid == 0 {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if spid == 0 {
		t.Fatal("supervisor pid never appeared")
	}
	sup := fmt.Sprintf("supervisor pid=%d", spid)
	t.Log(sup)

	// Find the orphaned sleep (the tool's backgrounded child) by matching its parent
	// chain: the sleep's grandparent was the tool, whose parent is the supervisor.
	// With the fix, the sleep's PPID should become the supervisor while the supervisor
	// lingers to reap it.
	deadline := time.Now().Add(3 * time.Second)
	adopted := false
	for time.Now().Before(deadline) {
		// Re-read the sleep pid each pass: it starts as a child of the tool shell, then
		// is orphaned when the shell exits and should reparent to the supervisor.
		entries, _ := os.ReadDir("/proc")
		for _, e := range entries {
			pid, err := strconv.Atoi(e.Name())
			if err != nil || pid <= 0 {
				continue
			}
			comm, err := os.ReadFile("/proc/" + e.Name() + "/comm")
			if err != nil || !strings.HasPrefix(strings.TrimSpace(string(comm)), "sleep") {
				continue
			}
			ppid, err := procPPID(t, pid)
			if err != nil {
				continue
			}
			if ppid == spid {
				adopted = true
				t.Logf("orphaned sleep pid=%d adopted by supervisor (ppid=%d)", pid, spid)
				break
			}
		}
		if adopted {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !adopted {
		t.Fatal("orphaned grandchild was never adopted by the supervisor - subreaper not effective")
	}

	// The supervisor lingers to reap the adopted orphan; once the sleep (0.8s) exits,
	// it must be gone from the table promptly - not defunct under init.
	exitDeadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(exitDeadline) {
		sleepGone := true
		entries, _ := os.ReadDir("/proc")
		for _, e := range entries {
			pid, err := strconv.Atoi(e.Name())
			if err != nil || pid <= 0 {
				continue
			}
			comm, err := os.ReadFile("/proc/" + e.Name() + "/comm")
			if err == nil && strings.HasPrefix(strings.TrimSpace(string(comm)), "sleep") {
				ppid, _ := procPPID(t, pid)
				if ppid == spid {
					sleepGone = false
					break
				}
			}
		}
		if sleepGone {
			t.Log("adopted orphan reaped by supervisor before supervisor exit")
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("adopted orphan still alive after sleep deadline - supervisor did not reap it")
}
