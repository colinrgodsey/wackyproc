package proc

const (
	ProcDirName           = ".proc"
	ToolsDirName          = "tools"
	MetaFileName          = "meta.json"
	PIDFileName           = "pid"
	SupervisorPIDFileName = "supervisor_pid"
	// SupervisedEnvVar is set in the environment of every tool process spawned by
	// wackyproc run. Its value is the supervising process's pid - it doubles as
	// provenance ("wackyproc spawned THIS process"). Downstream tools (e.g. wackypub's
	// agent prompt --async) gate on its presence to prove the dispatch is supervised
	// and its output is captured/retrievable, not fire-and-forget.
	SupervisedEnvVar = "WACKYPROC_SUPERVISED"
	PGIDFileName     = "pgid"
	ExitCodeFileName = "exit_code"
	CrashedFileName  = "crashed"
	StdinFileName    = "stdin"
	StdoutFileName   = "stdout"
	StderrFileName   = "stderr"
	StatusRunning    = "RUNNING"
	StatusCompleted  = "COMPLETED"
	StatusFailed     = "FAILED"
	StatusCrashed    = "CRASHED"
	// Deprecated: Crashed state is persisted via CrashedFileName instead of CrashedExitCode.
	CrashedExitCode = 137
	// Wait polling ramp (tasks/wackyproc/wait-polling-ramp): the first WaitPollRampCount
	// polls run at WaitPollStartIntervalMs so short tasks are still detected within a
	// couple of intervals of finishing, then the interval doubles per poll
	// (waitPollIntervalMs) until it settles at WaitPollSettleIntervalMs - a 10-minute
	// wait stops paying 20Hz CheckLiveness for a process that is clearly long-running.
	WaitPollStartIntervalMs  = 100
	WaitPollRampCount        = 10
	WaitPollSettleIntervalMs = 1000
	StopGracePeriodMs        = 3000
	IDLength                 = 8
	MaxIDGenerationRetries   = 100
	MaxWaitSeconds           = 500

	// MaxTerminalEntries caps how many terminal (COMPLETED, FAILED, CRASHED) process
	// records are retained. A retunable starting default, deliberately below the
	// 300-entry scratchpad cap since records carry captured output.
	MaxTerminalEntries = 100
)

// Meta records metadata about a spawned background process.
type Meta struct {
	ID          string   `json:"id"`
	Tool        string   `json:"tool"`
	ToolPath    string   `json:"tool_path"`
	Args        []string `json:"args"`
	Cwd         string   `json:"cwd"`
	StartedAt   int64    `json:"started_at"`
	StartTime   string   `json:"start_time,omitempty"`
	Gen         uint64   `json:"gen"`
	ConsumedSeq uint64   `json:"consumed_seq,omitempty"`
}

// ProcessInfo represents the user-visible status of a process. Args is deliberately
// EXCLUDED from JSON serialization (json:"-"): list --json must stay compact no matter how
// large the per-command args are (agent prompts, scratchpad hand-offs can be multi-KB;
// tasks/wackyproc/list-describe-split). Full args live in DescribeInfo, served by Describe.
type ProcessInfo struct {
	ID        string   `json:"id"`
	Tool      string   `json:"tool"`
	Args      []string `json:"-"`
	Status    string   `json:"status"`
	PID       int      `json:"pid,omitempty"`
	PGID      int      `json:"pgid,omitempty"`
	ExitCode  *int     `json:"exit_code,omitempty"`
	StartedAt int64    `json:"started_at"`
}

// DescribeInfo is the full-detail view of ONE process record, served by Describe and the
// wackyproc describe command. Unlike ProcessInfo it carries the complete Args plus the
// surrounding metadata (cwd, tool path, output locations, consumed state).
type DescribeInfo struct {
	ID         string   `json:"id"`
	Tool       string   `json:"tool"`
	ToolPath   string   `json:"tool_path"`
	Args       []string `json:"args"`
	Cwd        string   `json:"cwd"`
	Status     string   `json:"status"`
	PID        int      `json:"pid,omitempty"`
	PGID       int      `json:"pgid,omitempty"`
	ExitCode   *int     `json:"exit_code,omitempty"`
	StartedAt  int64    `json:"started_at"`
	Consumed   bool     `json:"consumed"`
	StdoutFile string   `json:"stdout_file"`
	StderrFile string   `json:"stderr_file"`
	StdinFile  string   `json:"stdin_file,omitempty"`
}
