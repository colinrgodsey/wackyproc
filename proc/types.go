package proc

const (
	ProcDirName               = ".proc"
	ToolsDirName              = "tools"
	MetaFileName              = "meta.json"
	PIDFileName               = "pid"
	SupervisorPIDFileName     = "supervisor_pid"
	PGIDFileName              = "pgid"
	ExitCodeFileName          = "exit_code"
	StdinFileName             = "stdin"
	StdoutFileName            = "stdout"
	StderrFileName            = "stderr"
	StatusRunning             = "RUNNING"
	StatusCompleted           = "COMPLETED"
	StatusFailed              = "FAILED"
	StatusCrashed             = "CRASHED"
	CrashedExitCode           = 137
	DefaultWaitPollIntervalMs = 50
	StopGracePeriodMs         = 3000
	IDLength                  = 4
	MaxIDGenerationRetries    = 100
)

// Meta records metadata about a spawned background process.
type Meta struct {
	ID        string   `json:"id"`
	Tool      string   `json:"tool"`
	ToolPath  string   `json:"tool_path"`
	Args      []string `json:"args"`
	Cwd       string   `json:"cwd"`
	StartedAt int64    `json:"started_at"`
	StartTime string   `json:"start_time,omitempty"`
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
