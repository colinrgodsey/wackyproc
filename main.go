package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"syscall"
	"text/tabwriter"

	"github.com/colinrgodsey/wackyproc/proc"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

var (
	negIntTokenPattern  = regexp.MustCompile(`^-\d+$`)
	flagErrTokenPattern = regexp.MustCompile(`(?:in |unknown flag: )(--?\S+)`)
)

// isAllDigits reports whether s is a non-empty string of ASCII digits.
// wackyproc uses it to tell the optional wait timeout apart from process IDs:
// record IDs are 8-character pronounceable slugs and can never be all digits.
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

//go:embed skills/wackyproc/SKILL.md
var bundledWackyprocSkill string

var (
	jsonOutput   bool
	describeJSON bool
	stopTimeout  int
	allWait      bool
	peekLines    int
)

var rootCmd = &cobra.Command{
	Use:   "wackyproc",
	Short: "Zero-daemon background process manager for turn-based agents",
	Long: `wackyproc is a self-supervising background process manager designed for
non-persistent, turn-based agents. It manages detached background processes,
tracking their lifecycle, process groups, and I/O streams on disk in .proc/.

All tools are resolved strictly from the agent's ./tools/ directory (no $PATH fallback).`,
}

var runCmd = &cobra.Command{
	Use:   "run <tool> [args...]",
	Short: "Spawn a tool in the background as a detached process",
	Long: `Spawn a tool from ./tools/<tool> as a detached background process.

Returns the allocated 4-character process ID immediately. The process runs
detached in its own process group and session, surviving the agent turn.
Any stdin passed to wackyproc is drained synchronously into .proc/<id>/stdin
before detaching.`,
	DisableFlagParsing: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 || (len(args) == 1 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help")) {
			return cmd.Help()
		}

		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get current working directory: %w", err)
		}

		toolName := args[0]
		toolArgs := args[1:]

		var stdinReader io.Reader
		stat, err := os.Stdin.Stat()
		if err == nil && (stat.Mode()&os.ModeCharDevice) == 0 {
			stdinReader = os.Stdin
		}

		procID, err := proc.Run(cwd, toolName, toolArgs, stdinReader)
		if err != nil {
			return err
		}

		fmt.Println(procID)
		return nil
	},
}

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List all tracked background processes and their current status",
	Long:  "Inspects .proc/ in the current directory and reports the status of all tracked processes.",
	RunE: func(cmd *cobra.Command, args []string) error {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get current working directory: %w", err)
		}

		list, err := proc.List(cwd)
		if err != nil {
			return err
		}

		if jsonOutput {
			data, err := json.MarshalIndent(list, "", "  ")
			if err != nil {
				return err
			}
			fmt.Println(string(data))
			return nil
		}

		if len(list) == 0 {
			fmt.Println("No tracked background processes.")
			return nil
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tSTATUS\tTOOL\tPID\tEXIT")
		for _, p := range list {
			exitStr := "-"
			if p.ExitCode != nil {
				exitStr = strconv.Itoa(*p.ExitCode)
			}
			pidStr := "-"
			if p.PID > 0 {
				pidStr = strconv.Itoa(p.PID)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", p.ID, p.Status, p.Tool, pidStr, exitStr)
		}
		return w.Flush()
	},
}

var waitCmd = &cobra.Command{
	Use:   "wait [seconds] <proc_id> [proc_id...]",
	Short: "Wait for background processes to reach a terminal state",
	Long: `Blocks up to the timeout waiting for the listed background processes to finish
(COMPLETED, FAILED, or CRASHED). At least one process ID is required: the old
bare any-mode wait is removed.

The first argument is the timeout in seconds when it looks like one
(a non-negative integer); process IDs are 8-character pronounceable slugs and
never do. The default timeout is the maximum wait.

Default (first-completed) semantics: returns as soon as ANY listed process
completes, printing the completed process ID. A listed process already terminal
at call time is returned immediately.

With --all, waits until ALL listed processes complete (the batch barrier) and
prints the last one to finish.

Returns the completed process ID and exits 0.
If the timeout expires before completion, exits non-zero.`,
	SilenceUsage: true,
	Args: func(cmd *cobra.Command, args []string) error {
		if err := cobra.MinimumNArgs(1)(cmd, args); err != nil {
			return err
		}
		if len(args) == 1 && negIntTokenPattern.MatchString(args[0]) {
			return fmt.Errorf("wait: timeout must be a non-negative integer (got %s)", args[0])
		}
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get current working directory: %w", err)
		}

		timeoutSeconds := proc.MaxWaitSeconds
		rest := args
		if isAllDigits(args[0]) {
			timeoutSeconds, err = strconv.Atoi(args[0])
			if err != nil {
				return fmt.Errorf("invalid timeout seconds %q: %w", args[0], err)
			}
			rest = args[1:]
		}
		if len(rest) == 0 {
			return fmt.Errorf("wait requires at least one process ID")
		}

		// Wire SIGINT/SIGTERM (agent turn cancel -> tool cancel -> wackyproc wait)
		// into WaitContext so a cancelled wait aborts promptly instead of stubbornly
		// waiting out the timeout or blocking in pidfd poll.
		waitCtx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()

		var procID string
		if allWait {
			procID, err = proc.WaitAllContext(waitCtx, cwd, timeoutSeconds, rest...)
		} else {
			procID, err = proc.WaitContext(waitCtx, cwd, timeoutSeconds, rest...)
		}
		if err != nil {
			return err
		}

		if procID == "" {
			if allWait {
				return fmt.Errorf("timeout waiting for all of %v", rest)
			}
			return fmt.Errorf("timeout waiting for process(es) %v", rest)
		}

		fmt.Println(procID)
		return nil
	},
}

var describeCmd = &cobra.Command{
	Use:   "describe \u003cproc_id\u003e [more ids...]",
	Short: "Show full details for one or more tracked processes",
	Long: `Shows full details for the specified process ID(s): complete command args, working
directory, tool path, status, pid/pgid, exit code, timestamps, captured-output file
locations. It does NOT print captured output (use get). With --json, emits a JSON array of the
records; without, prints a human-readable block per record.`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get current working directory: %w", err)
		}

		if describeJSON {
			infos := make([]*proc.DescribeInfo, 0, len(args))
			for _, procID := range args {
				info, err := proc.Describe(cwd, procID)
				if err != nil {
					return err
				}
				infos = append(infos, info)
			}
			data, err := json.MarshalIndent(infos, "", "  ")
			if err != nil {
				return err
			}
			fmt.Println(string(data))
			return nil
		}

		for _, procID := range args {
			info, err := proc.Describe(cwd, procID)
			if err != nil {
				return err
			}

			exitStr := "-"
			if info.ExitCode != nil {
				exitStr = strconv.Itoa(*info.ExitCode)
			}
			pidStr := "-"
			if info.PID > 0 {
				pidStr = strconv.Itoa(info.PID)
			}
			fmt.Printf("ID:        %s\n", info.ID)
			fmt.Printf("Tool:      %s\n", info.Tool)
			if info.ToolPath != "" {
				fmt.Printf("ToolPath:  %s\n", info.ToolPath)
			}
			fmt.Printf("Status:    %s\n", info.Status)
			fmt.Printf("PID:       %s\n", pidStr)
			if info.PGID > 0 {
				fmt.Printf("PGID:      %d\n", info.PGID)
			}
			fmt.Printf("Exit:      %s\n", exitStr)
			fmt.Printf("Cwd:       %s\n", info.Cwd)
			fmt.Printf("Started:   %d\n", info.StartedAt)
			fmt.Printf("Stdout:    %s\n", info.StdoutFile)
			fmt.Printf("Stderr:    %s\n", info.StderrFile)
			if info.StdinFile != "" {
				fmt.Printf("Stdin:     %s\n", info.StdinFile)
			}
			if len(info.Args) > 0 {
				fmt.Printf("Args:\n")
				for _, a := range info.Args {
					fmt.Printf("  %s\n", a)
				}
			} else {
				fmt.Printf("Args:      (none)\n")
			}
			fmt.Println()
		}
		return nil
	},
}

var getCmd = &cobra.Command{
	Use:   "get <proc_id>",
	Short: "Get stdout and stderr output for a background process",
	Long: `Dumps the captured stdout and stderr streams for the specified process ID
directly through wackyproc's own stdout and stderr.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get current working directory: %w", err)
		}

		procID := args[0]
		return proc.Get(cwd, procID, os.Stdout, os.Stderr)
	},
}

var peekCmd = &cobra.Command{
	Use:   "peek <proc_id> [--lines N]",
	Short: "Show the trailing lines of captured stdout and stderr for a process",
	Long: `Shows the trailing lines of captured stdout and stderr for the specified process ID
directly through wackyproc's own stdout and stderr without marking the record as consumed.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		// CLI-layer validation fires first to reject invalid flag inputs before resolving cwd or process state.
		if peekLines < 1 {
			return fmt.Errorf("--lines must be >= 1")
		}

		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get current working directory: %w", err)
		}

		procID := args[0]
		return proc.Peek(cwd, procID, peekLines, os.Stdout, os.Stderr)
	},
}

var stopCmd = &cobra.Command{
	Use:   "stop <proc_id>",
	Short: "Stop a running background process group",
	Long: `Stops the background process and its entire child process group using SIGTERM,
followed by SIGKILL if the process group does not terminate within the timeout.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get current working directory: %w", err)
		}

		procID := args[0]
		return proc.Stop(cwd, procID, stopTimeout)
	},
}

var killCmd = &cobra.Command{
	Use:   "kill <proc_id>",
	Short: "SIGKILL a running background process group immediately",
	Long: `Force-terminates the background process and its entire child process group with
SIGKILL, immediately and without the graceful SIGTERM-then-wait that stop performs. This
is the operator's response when a wedged process refuses to clean up (retry-storm, hung
model turn) and stop is too gentle.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get current working directory: %w", err)
		}
		return proc.Kill(cwd, args[0])
	},
}

var signalCmd = &cobra.Command{
	Use:   "signal <proc_id> <signal>",
	Short: "Send an arbitrary signal to a running background process group",
	Long: `Sends the given signal to the background process and its entire child process group.
The signal is required and may be numeric or named, case-insensitive, with the SIG- prefix
optional: 9, KILL, kill, and SIGKILL all mean the same thing. Supported names: HUP, INT,
QUIT, KILL, TERM, USR1, USR2, CONT, STOP, WINCH, plus any numeric value. Unknown names or
numbers are reported as errors, never silently defaulted.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		sig, err := proc.ParseSignal(args[1])
		if err != nil {
			return err
		}
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get current working directory: %w", err)
		}
		return proc.Signal(cwd, args[0], sig)
	},
}

var pruneCmd = &cobra.Command{
	Use:   "prune",
	Short: "Dispose all terminal background process records",
	Long:  "Disposes all terminal (COMPLETED, FAILED, CRASHED) process records and reports the removed IDs.",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get current working directory: %w", err)
		}

		return proc.Prune(cwd, os.Stdout)
	},
}

var removeCmd = &cobra.Command{
	Use:   "remove <proc_id>",
	Short: "Force-dispose a process record (any status) without waiting for process exit",
	Long: `Force-disposes the record of a stuck process: unlike prune, it works on RUNNING records,
	which prune can never clear because a process that died or hung outside the supervisor's
	capture window has no exit. The process itself is NOT signaled - if it is still alive it
	kkeeps running unmanaged, so stop or kill it first if you want it gone. The record's ID is
	recorded as disposed and will never be re-claimed.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get current working directory: %w", err)
		}

		toolName, err := proc.Remove(cwd, args[0])
		if err != nil {
			return err
		}
		fmt.Printf("removed %s (%s)\n", args[0], toolName)
		return nil
	},
}

var superviseCmd = &cobra.Command{
	Use:    "__supervise <proc_dir>",
	Short:  "Internal supervisor runner (hidden)",
	Hidden: true,
	Args:   cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		procDir := args[0]
		return proc.Supervise(procDir)
	},
}

var skillCmd = &cobra.Command{
	Use:   "skill",
	Short: "Print the wackyproc agent skill guide",
	Long:  "Prints the complete agent skill guide for wackyproc, including background proxy patterns and process management workflows.",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Print(bundledWackyprocSkill)
	},
}

func init() {
	listCmd.Flags().BoolVar(&jsonOutput, "json", false, "Output process list as JSON")
	describeCmd.Flags().BoolVar(&describeJSON, "json", false, "Output describe records as JSON")
	stopCmd.Flags().IntVar(&stopTimeout, "timeout", proc.DefaultStopTimeoutSeconds, "Seconds to wait after SIGTERM before sending SIGKILL")
	waitCmd.Flags().BoolVar(&allWait, "all", false, "Wait until ALL listed processes complete (default: first completed)")
	peekCmd.Flags().IntVar(&peekLines, "lines", 20, "Number of trailing lines of stdout and stderr to show")

	waitCmd.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		var token string
		var notExistErr *pflag.NotExistError
		if errors.As(err, &notExistErr) && notExistErr.GetSpecifiedShortnames() != "" {
			token = "-" + notExistErr.GetSpecifiedShortnames()
		} else if m := flagErrTokenPattern.FindStringSubmatch(err.Error()); len(m) > 1 {
			token = m[1]
		}
		if token != "" && negIntTokenPattern.MatchString(token) {
			return fmt.Errorf("wait: timeout must be a non-negative integer (got %s)", token)
		}
		return err
	})

	rootCmd.AddCommand(runCmd)
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(describeCmd)
	rootCmd.AddCommand(waitCmd)
	rootCmd.AddCommand(getCmd)
	rootCmd.AddCommand(peekCmd)
	rootCmd.AddCommand(stopCmd)
	rootCmd.AddCommand(killCmd)
	rootCmd.AddCommand(signalCmd)
	rootCmd.AddCommand(pruneCmd)
	rootCmd.AddCommand(removeCmd)
	rootCmd.AddCommand(skillCmd)
	rootCmd.AddCommand(superviseCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
