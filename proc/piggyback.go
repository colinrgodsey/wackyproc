package proc

import (
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
)

// The end-of-turn piggyback, D2 of tasks/wackyproc/notify-on-completion.
//
// wackypub runs end-of-turn hook scripts when a turn reaches its end and, on the paths that
// can still continue, honors a "continue" action by starting one more turn bounded by D88.
// That is the delivery channel for the completions D1's idle wake deliberately skips: an
// agent that was busy for the whole life of a job never saw it finish, and the supervisor's
// wake gate stepped aside rather than queue behind the live turn.
//
// The notified flag stays the single source of truth (P7). This path marks a job notified
// only when it actually hands the job to the model, and it reads the same flag D1 sets, so a
// job already surfaced by a manual read or by the idle wake is never reported twice.

// endOfTurnContinuationType is wackypub's HookActionContinue wire value. Restated here
// because the two repositories must not import each other.
const endOfTurnContinuationType = "continue"

// EndOfTurnPayload mirrors the JSON wackypub writes to the stdin of its end-of-turn hook
// scripts (pkg/agent EndOfTurnPayload). The field names are the wire contract.
type EndOfTurnPayload struct {
	AgentID           string `json:"agent_id"`
	SessionPath       string `json:"session_path"`
	StopReason        string `json:"stop_reason"`
	DurationMs        int64  `json:"duration_ms"`
	ContinuationCount int    `json:"continuation_count"`
	MaxContinuations  int    `json:"max_continuations"`
	CanContinue       bool   `json:"can_continue"`
}

// HookAction mirrors the control-flow block wackypub reads back from a hook's stdout JSON.
type HookAction struct {
	Type   string `json:"type"`
	Output string `json:"output,omitempty"`
	Stderr string `json:"stderr,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// HookOutput mirrors the stdout contract of a wackypub hook script.
type HookOutput struct {
	Action *HookAction `json:"action,omitempty"`
}

// PiggybackResult is what one piggyback evaluation decided. Surfaced holds the records this
// call handed to the model and marked notified; Skipped is the audit line for everything
// else, written so an operator can tell "nothing finished" from "finished but the turn could
// not take the hand-off".
type PiggybackResult struct {
	Output   HookOutput
	Surfaced []string
	Skipped  string
}

// ListPending returns the terminal records in cwd whose completion has not been surfaced by
// any path yet. It is the same list the idle wake and a manual read compete over, and it is
// what an operator sweeps after a crash, so it stays deliberately dumb: terminal, not yet
// notified, nothing else.
func ListPending(cwd string) ([]ProcessInfo, error) {
	list, err := List(cwd)
	if err != nil {
		return nil, err
	}
	pending := make([]ProcessInfo, 0, len(list))
	for _, p := range list {
		if p.Notified || !isTerminal(p.Status) {
			continue
		}
		pending = append(pending, p)
	}
	return pending, nil
}

// EndOfTurnPiggyback turns a wackypub end-of-turn payload into the hook output to return.
//
// The load-bearing rule is the CanContinue gate. Nine of wackypub's ten end-of-turn fire
// sites discard the chain result with can_continue false - aborted turns, the failure
// breaker, deferred images, an exhausted budget - so a continuation returned there is
// dropped. Marking a job notified on such a path would announce it to nobody and lose it, so
// this returns no action and leaves every record un-notified for the next chance.
//
// Nothing here may fail a turn (P5): an unparseable payload, an unreadable record directory
// or a refused mark all produce an empty result with a reason, never an error.
func EndOfTurnPiggyback(cwd string, payloadJSON []byte, stderr io.Writer) PiggybackResult {
	var payload EndOfTurnPayload
	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		return PiggybackResult{Skipped: fmt.Sprintf("unparseable end-of-turn payload: %v", err)}
	}
	pending, err := ListPending(cwd)
	if err != nil {
		return PiggybackResult{Skipped: fmt.Sprintf("cannot read process records: %v", err)}
	}
	if len(pending) == 0 {
		return PiggybackResult{Skipped: "no un-notified completed jobs"}
	}
	if !payload.CanContinue {
		return PiggybackResult{Skipped: fmt.Sprintf(
			"notify-only end-of-turn path (stop_reason=%q, continuation %d of %d): %d job(s) left un-notified for the next turn",
			payload.StopReason, payload.ContinuationCount, payload.MaxContinuations, len(pending))}
	}

	surfaced := make([]string, 0, len(pending))
	for _, p := range pending {
		if err := MarkNotified(filepath.Join(cwd, ProcDirName, p.ID)); err != nil {
			fmt.Fprintf(stderr, "warning: piggyback could not mark %s notified: %v\n", p.ID, err)
			continue
		}
		surfaced = append(surfaced, p.ID)
	}
	if len(surfaced) == 0 {
		return PiggybackResult{Skipped: "no pending job could be marked, nothing handed over"}
	}
	return PiggybackResult{
		Output: HookOutput{Action: &HookAction{
			Type:   endOfTurnContinuationType,
			Reason: "wackyproc-piggyback",
			Output: PiggybackReport(pending, surfaced),
		}},
		Surfaced: surfaced,
	}
}

// PiggybackReport renders the hand-off text the woken turn reads. It names every job and
// its own retrieval command (P9): a turn that is told "some jobs finished" without ids and a
// way to fetch results is not actionable. Jobs that failed to mark are named too, because
// the next turn will report them again and that should be explicable.
func PiggybackReport(pending []ProcessInfo, surfaced []string) string {
	surfacedSet := make(map[string]bool, len(surfaced))
	for _, id := range surfaced {
		surfacedSet[id] = true
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[wackyproc] %d background job(s) you started finished while this turn was running, and this turn continues to handle them.\n", len(pending))
	for i, p := range pending {
		tool := p.Tool
		if tool == "" {
			tool = "(unknown)"
		}
		fmt.Fprintf(&b, "\n  id:      %s\n", p.ID)
		fmt.Fprintf(&b, "  tool:    %s\n", tool)
		fmt.Fprintf(&b, "  status:  %s\n", p.Status)
		if p.ExitCode != nil {
			fmt.Fprintf(&b, "  exit:    %d\n", *p.ExitCode)
		}
		fmt.Fprintf(&b, "  result:  wackyproc get %s\n", p.ID)
		fmt.Fprintf(&b, "  detail:  wackyproc describe %s\n", p.ID)
		if !surfacedSet[p.ID] {
			b.WriteString("  note:    this job could not be marked notified, so it may be reported again\n")
		}
		if i == len(pending)-1 {
			b.WriteString("\nRead them with the commands above; captured output stays available until the record is retired.\n")
		}
	}
	return b.String()
}

// WriteHookOutput writes the hook's stdout JSON, which is always an object even when empty,
// because wackypub parses whatever the script prints.
func WriteHookOutput(w io.Writer, out HookOutput) error {
	data, err := json.Marshal(out)
	if err != nil {
		return fmt.Errorf("failed to encode hook output: %w", err)
	}
	_, err = fmt.Fprintln(w, string(data))
	return err
}
