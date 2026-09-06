package proc

const (
	ProcDirName           = ".proc"
	ToolsDirName          = "tools"
	MetaFileName          = "meta.json"
	PIDFileName           = "pid"
	SupervisorPIDFileName = "supervisor_pid"
	PGIDFileName          = "pgid"
	ExitCodeFileName      = "exit_code"
	CrashedFileName       = "crashed"
	StdinFileName         = "stdin"
	StdoutFileName        = "stdout"
	StderrFileName        = "stderr"
	StatusRunning         = "RUNNING"
	StatusCompleted       = "COMPLETED"
	StatusFailed          = "FAILED"
	StatusCrashed         = "CRASHED"
	// Deprecated: Crashed state is persisted via CrashedFileName instead of CrashedExitCode.
	CrashedExitCode           = 137
	DefaultWaitPollIntervalMs = 50
	StopGracePeriodMs         = 3000
	IDLength                  = 4
	MaxIDGenerationRetries    = 100
	MaxWaitSeconds            = 500

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

// ProcessInfo represents the user-visible status of a process.
type ProcessInfo struct {
	ID        string   `json:"id"`
	Tool      string   `json:"tool"`
	Args      []string `json:"args"`
	Status    string   `json:"status"`
	PID       int      `json:"pid,omitempty"`
	PGID      int      `json:"pgid,omitempty"`
	ExitCode  *int     `json:"exit_code,omitempty"`
	StartedAt int64    `json:"started_at"`
}
