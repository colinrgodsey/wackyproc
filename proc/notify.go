package proc

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// Completion notification, D1 of tasks/wackyproc/notify-on-completion.
//
// wackyproc has no daemon, so the only moment anything notices that a job finished is the
// supervisor's own exit. At that instant it looks for a wackyproc-owned hook in the owning
// agent's folder, hooks/on-proc-complete/, discovered with the same convention wackypub
// applies to its own hook events, and runs each script detached.
//
// The gate is deliberately skip-not-queue. A wake queued behind the agent's live turn would
// stack a second turn to deliver information the end-of-turn piggyback path can hand over
// inside the turn already running. Skipping costs nothing: the record stays un-notified, so
// a manual get/wait read, or the piggyback path once it exists, still surfaces it.
//
// Nothing here may block the supervisor or change its exit status. A missing directory, a
// refused lock probe, an unwritable notice, a broken script and an unforkable child all mean
// the same thing - the agent was not woken - which is precisely the state a manual read
// recovers from.
const (
	// CompletionHookEvent is the wackyproc-owned hook event. wackypub core never learns
	// it exists: only wackyproc looks for this directory.
	CompletionHookEvent = "on-proc-complete"

	hooksDirName        = "hooks"
	noticeFileName      = "notice.txt"
	hookLogFileName     = "hook.log"
	sessionLockFileName = "session.lock"

	// AgentDirEnv names the owning agent's folder. It is read at run time for callers
	// whose cwd is not the agent folder, and handed back to hook scripts so a script can
	// address the same agent without guessing.
	AgentDirEnv = "WACKYPROC_AGENT_DIR"

	HookEnvProcID     = "WACKYPROC_PROC_ID"
	HookEnvTool       = "WACKYPROC_TOOL"
	HookEnvStatus     = "WACKYPROC_STATUS"
	HookEnvExitCode   = "WACKYPROC_EXIT_CODE"
	HookEnvStdoutFile = "WACKYPROC_STDOUT_FILE"
	HookEnvStderrFile = "WACKYPROC_STDERR_FILE"
	HookEnvNoticeFile = "WACKYPROC_NOTICE_FILE"
	HookEnvBin        = "WACKYPROC_BIN"
)

// errNoAgentDir marks a record that cannot be attributed to an agent, so no hook can be
// found for it. Callers degrade to "not woken".
var errNoAgentDir = errors.New("no owning agent folder recorded")

// AgentFolder is the folder whose hooks/ and session.lock belong to the agent that owns this
// record: the recorded override when there is one, otherwise the cwd the job was started
// from, which is the agent's own directory under wackypub tool dispatch.
func (m Meta) AgentFolder() string {
	if m.AgentDir != "" {
		return m.AgentDir
	}
	return m.Cwd
}

// DiscoverCompletionHooks lists executable scripts in <agentDir>/hooks/on-proc-complete/ in
// ascending leading-number order, matching wackypub's hook-chain ordering. An absent
// directory is not an error: no hook registered is the normal case and changes nothing.
func DiscoverCompletionHooks(agentDir string) ([]string, error) {
	if agentDir == "" {
		return nil, nil
	}
	hookDir := filepath.Join(agentDir, hooksDirName, CompletionHookEvent)
	entries, err := os.ReadDir(hookDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read hooks dir %s: %w", hookDir, err)
	}
	var scripts []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(hookDir, entry.Name())
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
			continue
		}
		scripts = append(scripts, path)
	}
	sort.Slice(scripts, func(i, j int) bool {
		baseI := filepath.Base(scripts[i])
		baseJ := filepath.Base(scripts[j])
		numI, hasI := leadingNumber(baseI)
		numJ, hasJ := leadingNumber(baseJ)
		if hasI && hasJ {
			if numI != numJ {
				return numI < numJ
			}
			return baseI < baseJ
		}
		if hasI != hasJ {
			return hasI
		}
		return baseI < baseJ
	})
	return scripts, nil
}

func leadingNumber(name string) (int64, bool) {
	var digits []rune
	for _, r := range name {
		if unicode.IsDigit(r) {
			digits = append(digits, r)
		} else {
			break
		}
	}
	if len(digits) == 0 {
		return 0, false
	}
	n, err := strconv.ParseInt(string(digits), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// CompletionNotice is the text a woken turn is handed. It names the job, the outcome and the
// exact retrieval command: a turn that starts with no way to reconstruct why it started is a
// failure even when the mechanism works.
func CompletionNotice(meta Meta, status string, exitCode *int) string {
	var b strings.Builder
	b.WriteString("[wackyproc] A background job you started finished, and nothing else was running, so this turn woke up to handle it.\n")
	fmt.Fprintf(&b, "  id:      %s\n", meta.ID)
	tool := meta.Tool
	if tool == "" {
		tool = "(unknown)"
	}
	fmt.Fprintf(&b, "  tool:    %s\n", tool)
	fmt.Fprintf(&b, "  status:  %s\n", status)
	if exitCode != nil {
		fmt.Fprintf(&b, "  exit:    %d\n", *exitCode)
	}
	fmt.Fprintf(&b, "  result:  wackyproc get %s\n", meta.ID)
	fmt.Fprintf(&b, "  detail:  wackyproc describe %s\n", meta.ID)
	return b.String()
}

// HookFireResult reports one completion-hook decision. Skipped is empty only when at least
// one script started. Every reason is also written to the record's hook.log, so an operator
// can reconstruct why no wake happened.
type HookFireResult struct {
	Fired   []string
	Skipped string
	// Quiet marks a skip that is the ordinary outcome of an unconfigured or already
	// handled job. Those verdicts are not worth a log line on every process record;
	// the interesting ones (busy agent, failed probe, broken script) are not quiet.
	Quiet bool
}

// MaybeFireCompletionHooks is the idle-wake gate. The supervisor calls it as its last act,
// after the terminal state is on disk; status and exit code are read back from the record so
// the gate describes what an operator would see, not what the supervisor remembered.
func MaybeFireCompletionHooks(meta Meta, procDir string) HookFireResult {
	agentDir := meta.AgentFolder()
	if agentDir == "" {
		return HookFireResult{Skipped: errNoAgentDir.Error(), Quiet: true}
	}
	if isNotified(procDir) {
		return HookFireResult{Skipped: "already notified", Quiet: true}
	}
	scripts, err := DiscoverCompletionHooks(agentDir)
	if err != nil {
		return HookFireResult{Skipped: err.Error()}
	}
	if len(scripts) == 0 {
		return HookFireResult{Skipped: "no " + CompletionHookEvent + " hooks registered", Quiet: true}
	}
	busy, err := SessionLockBusy(agentDir)
	if err != nil {
		return HookFireResult{Skipped: fmt.Sprintf("session lock probe failed: %v", err)}
	}
	if busy {
		return HookFireResult{Skipped: "agent is mid-turn: wake skipped rather than queued, job left un-notified"}
	}

	status := StatusCrashed
	var exitCode *int
	if liveness, err := CheckLiveness(procDir, &meta); err == nil {
		status = liveness.Status
		exitCode = liveness.ExitCode
	}

	notice := CompletionNotice(meta, status, exitCode)
	noticePath := filepath.Join(procDir, noticeFileName)
	if err := os.WriteFile(noticePath, []byte(notice), 0o644); err != nil {
		return HookFireResult{Skipped: fmt.Sprintf("cannot write completion notice: %v", err)}
	}

	env, logPath := hookEnv(meta, status, exitCode, procDir, noticePath)
	fired := make([]string, 0, len(scripts))
	for _, script := range scripts {
		if err := startHookDetached(script, env, notice, logPath); err != nil {
			appendHookLog(logPath, fmt.Sprintf("script %s could not start: %v", script, err))
			continue
		}
		fired = append(fired, script)
	}
	if len(fired) == 0 {
		return HookFireResult{Skipped: "no hook script could be started"}
	}
	// Marking here means "a wake was dispatched", not "the wake turn succeeded". The
	// supervisor cannot wait for a hook it does not observe, so a hook that starts and then
	// dies is a lost wake with a hook.log line beside it rather than a re-fire storm.
	if err := MarkNotified(procDir); err != nil {
		appendHookLog(logPath, fmt.Sprintf("hook fired but the notified flag could not be written: %v", err))
	}
	appendHookLog(logPath, fmt.Sprintf("woke agent %s for job %s (%s) via %d script(s)", agentDir, meta.ID, status, len(fired)))
	return HookFireResult{Fired: fired}
}

func hookEnv(meta Meta, status string, exitCode *int, procDir, noticePath string) ([]string, string) {
	exit := ""
	if exitCode != nil {
		exit = strconv.Itoa(*exitCode)
	}
	env := []string{
		AgentDirEnv + "=" + meta.AgentFolder(),
		HookEnvProcID + "=" + meta.ID,
		HookEnvTool + "=" + meta.Tool,
		HookEnvStatus + "=" + status,
		HookEnvExitCode + "=" + exit,
		HookEnvStdoutFile + "=" + filepath.Join(procDir, StdoutFileName),
		HookEnvStderrFile + "=" + filepath.Join(procDir, StderrFileName),
		HookEnvNoticeFile + "=" + noticePath,
	}
	if bin, err := os.Executable(); err == nil {
		env = append(env, HookEnvBin+"="+bin)
	}
	return env, filepath.Join(procDir, hookLogFileName)
}

// startHookDetached runs one hook script in its own session so it outlives the supervisor,
// with the notice text on stdin the way wackypub hands text to a hook chain. Output goes to
// the record's hook.log, never to the job's own stdout/stderr, so waking an agent cannot
// corrupt the result somebody else reads with get.
func startHookDetached(script string, env []string, notice, logPath string) error {
	logFile, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd := exec.Command(script)
	cmd.Dir = filepath.Dir(logPath)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = strings.NewReader(notice)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	detachProcess(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	// Deliberately not waited for: the supervisor is the last process that knows this job
	// exists and it must exit on schedule. The hook is reparented, so never calling Wait
	// leaks nothing beyond an unreaped child of a process that is about to exit.
	return nil
}

func appendHookLog(logPath, line string) {
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, line)
}

// MarkNotified records that this job's completion has been surfaced to its owning agent. It
// is the single source of truth shared by all notification paths: a manual get/wait read,
// the idle wake, and the end-of-turn piggyback once that lands.
func MarkNotified(procDir string) error {
	meta, err := readMeta(procDir)
	if err != nil {
		return err
	}
	if meta.Notified {
		return nil
	}
	meta.Notified = true
	return writeMeta(procDir, meta)
}

func isNotified(procDir string) bool {
	meta, err := readMeta(procDir)
	if err != nil {
		return false
	}
	return meta.Notified
}

// writeMeta rewrites meta.json through a temp file and rename, matching the durability
// precedent in id.go and seqlock.go. Losing meta.json would strand a record that a
// supervisor may still be signalling, so a half-written file is never left in place.
func writeMeta(procDir string, meta Meta) error {
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to serialize %s: %w", MetaFileName, err)
	}
	path := filepath.Join(procDir, MetaFileName)
	tmp, err := os.CreateTemp(procDir, MetaFileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("failed to create temp meta: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("failed to write temp meta: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to close temp meta: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to replace %s: %w", MetaFileName, err)
	}
	return nil
}

// markReadNotified marks every listed ID that has reached a terminal state, used by
// wait, whose successful return hands the caller a completed job. IDs whose record cannot
// be read or written are skipped: the worst case is that a later wake path surfaces that
// job once, which is strictly better than losing a completion.
func markReadNotified(cwd string, ids []string) {
	for _, id := range ids {
		if id == "" {
			continue
		}
		procDir := filepath.Join(cwd, ProcDirName, id)
		meta, err := readMeta(procDir)
		if err != nil {
			continue
		}
		if liveness, liveErr := CheckLiveness(procDir, &meta); liveErr != nil || !isTerminal(liveness.Status) {
			continue
		}
		_ = MarkNotified(procDir)
	}
}
