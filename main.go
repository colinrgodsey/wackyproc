package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/colinrgodsey/wackyproc/proc"
	"github.com/spf13/cobra"
)

//go:embed skills/wackyproc/SKILL.md
var bundledWackyprocSkill string

var (
	jsonOutput  bool
	stopTimeout int
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
	Use:   "wait <seconds>",
	Short: "Wait for any background process to reach a terminal state",
	Long: `Blocks up to the specified timeout (in seconds) waiting for ANY tracked
background process to finish (COMPLETED, FAILED, or CRASHED).

Returns the process ID of whichever process completes first.
If the timeout expires before any process finishes, outputs nothing.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cwd, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("failed to get current working directory: %w", err)
		}

		seconds, err := strconv.Atoi(args[0])
		if err != nil {
			return fmt.Errorf("invalid timeout seconds %q: %w", args[0], err)
		}

		procID, err := proc.Wait(cwd, seconds)
		if err != nil {
			return err
		}

		if procID != "" {
			fmt.Println(procID)
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
	stopCmd.Flags().IntVar(&stopTimeout, "timeout", 3, "Seconds to wait after SIGTERM before sending SIGKILL")

	rootCmd.AddCommand(runCmd)
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(waitCmd)
	rootCmd.AddCommand(getCmd)
	rootCmd.AddCommand(stopCmd)
	rootCmd.AddCommand(skillCmd)
	rootCmd.AddCommand(superviseCmd)
}

func main() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
