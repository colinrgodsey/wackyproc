# wackyproc

A zero-daemon background process manager companion tool for turn-based, non-persistent AI agents.

In turn-based agent runtimes (like [wackypub](https://github.com/colinrgodsey/wackypub)), an agent process only lives for the duration of a single turn. `wackyproc` allows agents to spawn long-running background tasks (builds, test suites, servers) and check back on them across turns without requiring a master daemon or persistent background service.

## Core Design Principles

1. **Self-Supervising Detached Runner**: Spawns detached processes via `Setsid: true`, isolating the target child in its own process group (`Setpgid: true`). No daemon to install, no socket to connect to — supervision is a re-exec of the same binary (`__supervise`, hidden). On Linux, the detached supervisor also becomes a child subreaper (`PR_SET_CHILD_SUBREAPER`) so a tool's own grandchildren (e.g. a bash wrapper spawning another process) reparent to the supervisor instead of PID 1 if orphaned; the supervisor lingers up to a bounded 30s grace period after the tool exits, reaping any adopted orphans before it exits itself, so a completed background task doesn't leave defunct processes behind. (No-op on non-Linux. In a container whose PID 1 doesn't reap - e.g. plain bash - the supervisor's own eventual zombie still needs an external reaping init such as `tini`; this is the in-process half of that fix only.)
2. **State on Disk (`.proc/<id>/`)**: Each process state is tracked in a dedicated directory containing `meta.json`, `pid`, `pgid`, `supervisor_pid`, `stdin`, `stdout`, `stderr`, and `exit_code`.
3. **Strict Tool Resolution**: Resolves commands strictly against `<cwd>/tools/<tool>` with **no `$PATH` fallback**, matching agent capability boundaries.
4. **Zero Scratchpad Format Coupling**: Dumps captured output via `wackyproc get <proc_id>`, allowing the host harness to handle auto-capture generically without coupling to internal formats.
5. **PID Reuse Protection**: Validates process start times against recorded metadata to detect PID recycling after abrupt crashes.

## Commands

- `wackyproc run <tool> [args...]`: Spawns `./tools/<tool>` as a detached background process and outputs its 8-character pronounceable slug ID (e.g. `katoruvo`). Stdin piped into `run` is drained into `.proc/<id>/stdin` before detaching.
- `wackyproc list [--json]`: Lists all tracked processes (`ID STATUS TOOL PID EXIT`) and their current status (`RUNNING`, `COMPLETED`, `FAILED`, `CRASHED`). `--json` is COMPACT by contract: it returns only the table fields plus timestamps and the completion-notification flag (`id`, `tool`, `status`, `pid`, `pgid`, `exit_code`, `started_at`, `notified`) and deliberately EXCLUDES command args - a dispatch's args can be multi-KB (agent prompts), and list is the status snapshot; use `describe` for full args.
- `wackyproc describe <proc_id> [more ids...] [--json]`: Full detail for one or more records - complete command args, cwd, tool path, output-file locations, and `notified` (in `--json`).
- `wackyproc wait [seconds] <proc_id> [proc_id...] [--all]`: Blocks up to N seconds (default 500; the first argument is the timeout only when it is all digits, since record IDs are pronounceable slugs and never numeric) and returns the ID of the first listed process to reach a terminal state. A process already terminal at entry is returned immediately, and a listed ID that does not exist fails immediately. With `--all` the call is a barrier: it waits for every listed process and returns the last one to complete. Returning a terminal process marks that record `notified: true` (or all listed records under `--all`) so completion hooks do not re-notify the agent. A timeout exits non-zero without returning an ID. On Linux, waiting arms one `pidfd` per listed running process for near-instant, zero-polling exit detection; elsewhere (or if `pidfd` fails mid-wait) it falls back to polling that ramps from 100ms to 1000ms after the first 10 polls.
- `wackyproc get <proc_id>`: Dumps captured stdout and stderr to the terminal. When the record is in a terminal state (`COMPLETED`, `FAILED`, `CRASHED`), `get` marks it `notified: true` in `meta.json` so neither the idle-wake hook nor the end-of-turn piggyback hook surfaces that completion again. Reading partial output from a still-`RUNNING` job leaves `notified` unchanged, and reading output has no effect on record retention/lifetime. A record whose `meta.json` has been disposed concurrently is reported as not found.
- `wackyproc peek <proc_id> [--lines N]`: Shows the trailing N lines (default 20) of captured stdout/stderr without a full dump. Pure read: it writes no state and never sets `notified`.
- `wackyproc end-of-turn`: Reads a wackypub `end-of-turn` hook payload from stdin and writes `HookOutput` JSON to stdout (used by `examples/hooks/end-of-turn/20-wackyproc-piggyback`, not for interactive invocation). When `can_continue` is `true` and un-notified terminal records exist, marks them `notified: true` and returns a `continue` action naming the completed jobs; otherwise prints `{}` and leaves pending records un-notified.
- `wackyproc prune`: Disposes all terminal process records and reports the removed IDs.
- `wackyproc remove <proc_id>`: Force-disposes a record regardless of status - the escape hatch for stuck `RUNNING` records that `prune` can never clear (a process that died or hung outside the supervisor's capture window has no exit). The process is NOT signaled: if it is still alive it keeps running unmanaged, so stop or kill it first if you want it gone.
- `wackyproc stop <proc_id> [--timeout N]`: Gracefully stops the whole process group via `SIGTERM`, falling back to `SIGKILL` after N seconds (default 3).
- `wackyproc kill <proc_id>`: Immediately terminates the whole process group with `SIGKILL`.
- `wackyproc signal <proc_id> <signal>`: Sends a signal to the process group, by name (case-insensitive, `SIG-` prefix optional - `KILL`, `kill`, and `SIGKILL` all mean the same thing) or number. Supported names: `HUP`, `INT`, `QUIT`, `KILL`, `TERM`, `USR1`, `USR2`, `CONT`, `STOP`, `WINCH`, plus any numeric signal value. An unknown name or number is reported as an error, never silently defaulted.
- `wackyproc skill`: Prints the bundled agent skill guide (`skills/wackyproc/SKILL.md`, embedded in the binary).

## Record retention and status taxonomy

Reading output has no effect on a record's retention lifetime: a record's output may be read any number of times with `get` or `peek`, and terminal records are auto-retired strictly by age when their count exceeds the cap (`MaxTerminalEntries = 100`, deliberately below the 300-entry scratchpad cap since records carry captured output). Retirement is age-based - the oldest `Gen` goes first, regardless of whether the output was ever read or notified - and runs at both growth points: every `run` and every `list` (before the snapshot, so a record the cap is about to drop never appears in that call's results). Each retirement is logged to stderr; `prune` disposes terminal records on demand.
`remove` disposes a specific record in any state. Every disposal path records the ID in a disposed list, and ID generation never re-claims a disposed ID, so a fresh dispatch cannot inherit an ID that logs still reference from the previous record.

### The `notified` flag

Each record's `meta.json` carries a boolean `notified` field (`"notified": true`, omitted when `false`). It starts `false` when the job is spawned and serves as the single source of truth shared across all three completion-delivery paths (P7):

1. **Manual read (`wackyproc get` / `wackyproc wait`)**: Calling `get` on a terminal job, or `wait` returning a terminal job (all listed jobs under `--all`), marks the record `notified: true`. Pure inspection commands (`peek`, `list`, `describe`, or `get` while a job is still `RUNNING`) never set `notified`.
2. **Idle wake (`on-proc-complete` hook at supervisor exit)**: Marks the record `notified: true` once at least one `hooks/on-proc-complete/` script is started while the owning agent's `session.lock` is free.
3. **End-of-turn piggyback (`wackyproc end-of-turn` / `20-wackyproc-piggyback`)**: Marks pending terminal records `notified: true` when handing them to a continuation turn (`can_continue: true`).

Because all three paths check and atomically update the same `notified` flag (`proc.MarkNotified`, written via temp file + rename), no two paths ever surface the same completed job twice, and a manual `get` or `wait` read suppresses both hook paths. `notified` is visible in `wackyproc list --json` and `wackyproc describe --json`.

Status is derived from liveness plus exit code, never stored:

- `RUNNING` — PID alive in its recorded process group.
- `COMPLETED` — exited 0.
- `FAILED` — exited non-zero.
- `CRASHED` — no exit code recorded but the process is gone (kill -9, OOM, abrupt host crash; exit 137 from SIGKILL also lands here). Start-time validation guards against PID reuse masquerading as liveness.

## Supervised execution

Every tool process spawned by `wackyproc run` inherits
`WACKYPROC_SUPERVISED=<supervisor-pid>` - an attestation that the process is
supervised by wackyproc (output captured, retrievable, not fire-and-forget),
with the supervisor's pid doubling as provenance. Downstream tools (e.g.
`wackypub agent prompt --async`) gate on its presence.

To dispatch an async, supervised A2A call and later retrieve the output:

```bash
wackyproc run wackypub agent <target> prompt --async "...NO_RESPONSE..."
wackyproc wait <proc_id>
wackyproc get <proc_id>   # stdout holds the target's model output
```

Note that `WACKYPROC_SUPERVISED` + `--async` is for **cross-agent** supervised dispatch (where `--async` relaxes A2A call-chain cycle detection for that hop) and is distinct from `on-proc-complete` **self-wake** (below), which starts a fresh top-level turn on the owning agent's own ID with inherited chain variables stripped and deliberately does not use `--async` or `WACKYPROC_SUPERVISED`.

There is no deadline machinery by design: a hung supervised process holds
the same locks and has the same risk profile as a hung normal call, so the
remedy is the existing kill signals - `wackyproc stop <id>` (SIGTERM, then
SIGKILL after the grace period) or `wackyproc kill <id>` (immediate
SIGKILL of the whole process group).

## Hooks

`wackyproc` ships three opt-in hook scripts under `examples/hooks/` that pair with `wackypub`'s agent `hooks/` directory convention (`<agentFolder>/hooks/<event>/<NN-name>`):

| Script | Event | Owner | Purpose |
|---|---|---|---|
| `examples/hooks/pre-run-command/10-auto-defer` | `pre-run-command` | `wackypub` | Defers qualifying `run_command` tool calls into `wackyproc run` background jobs according to `WACKYPROC_AUTODEFER_POLICY`. |
| `examples/hooks/on-proc-complete/10-wake-agent` | `on-proc-complete` | `wackyproc` | Wakes an **idle** owning agent with a fresh turn when a background job finishes (`session.lock` free; skips when busy). |
| `examples/hooks/end-of-turn/20-wackyproc-piggyback` | `end-of-turn` | `wackypub` | Piggybacks un-notified completions that finished while the agent was mid-turn onto an end-of-turn continuation (`wackyproc end-of-turn`). |

`wackypub` core remains completely unaware of `wackyproc` (P1): `wackypub` provides generic `pre-run-command` and `end-of-turn` hook events, while `wackyproc` alone discovers and fires `on-proc-complete`.

### 1. Auto-defer (`pre-run-command/10-auto-defer`)

Consumes `wackypub`'s `pre-run-command` event and can hand a `run_command` call to `wackyproc run` instead of letting it execute inline.

Install (strictly opt-in per agent):

```bash
cp examples/hooks/pre-run-command/10-auto-defer <agentFolder>/hooks/pre-run-command/
chmod +x <agentFolder>/hooks/pre-run-command/10-auto-defer
```

When the policy decides to defer, the call is spawned through `wackyproc run` (same tool, same args, agent folder as cwd) and the hook returns `{"action": {"type": "short_circuit", "reason": "auto-defer (<policy>)", "output": "..."}}` so the model receives the 8-character proc ID and retrieval commands instead of blocking:

```text
[wackyproc auto-defer] This command was not run inline; it is now background job <id>.
  retrieve result:  wackyproc get <id>
  wait for it:      wackyproc wait 30 <id>
  peek at output:   wackyproc peek <id>
```

#### Deferral policy (`WACKYPROC_AUTODEFER_POLICY`)

The decision is an explicit, documented parameter, not a buried heuristic:

```text
WACKYPROC_AUTODEFER_POLICY=off       never defer (kill switch)
WACKYPROC_AUTODEFER_POLICY=marker    only calls the caller marked WACKYPROC_BG=1
                                     in the run_command env (default when unset)
WACKYPROC_AUTODEFER_POLICY=heuristic calls matching the explicit list in the script
WACKYPROC_AUTODEFER_POLICY=always    every call
```

Which policy to run is a product decision (card `tasks/wackyproc/hook-integration-auto-defer-background`, P10); the heuristic list (`cargo`, `make`, `mvn`, `gradle`, `docker`, `pip`, `pip3`, `go test|build|run`, and `bash -c` / `sh -c` invocations starting with common build/test commands) lives in the script on purpose, so it is edited in one place. The `wackyproc` and `wackypub` tools are never deferred under any policy.

#### Failure behavior

Any failure (no `python3`, unreadable payload, no `wackyproc`, spawn error) makes the hook exit 0 with no action and the command simply runs inline as if the hook were not installed.

### 2. Idle wake on completion (`on-proc-complete/10-wake-agent`)

`on-proc-complete` is a `wackyproc`-specific hook event discovered under the owning agent's `<agentFolder>/hooks/on-proc-complete/` directory (regular executable files sorted in ascending numeric order by leading digits, matching `wackypub`'s hook convention). The owning agent folder is `WACKYPROC_AGENT_DIR` when set at `wackyproc run` time, or the `cwd` from which the job was started.

Install:

```bash
cp examples/hooks/on-proc-complete/10-wake-agent <agentFolder>/hooks/on-proc-complete/
chmod +x <agentFolder>/hooks/on-proc-complete/10-wake-agent
```

- **Fire point**: `wackyproc`'s detached supervisor calls `MaybeFireCompletionHooks` as its final action after recording the job's terminal state (`COMPLETED`, `FAILED`, or `CRASHED`).
- **Skip-not-queue idle-wake gate (P6)**:
  - If the record is already `notified: true` (e.g. a manual `wait` or `get` read it first) or no `on-proc-complete` hooks are installed, the supervisor exits quietly without creating a `hook.log`.
  - Otherwise, the supervisor probes `<agentFolder>/session.lock` with a non-blocking exclusive flock (`LOCK_EX|LOCK_NB`):
    - **Lock free (agent is idle)**: Writes the formatted completion notice to `.proc/<id>/notice.txt`, starts each hook script detached in its own session (`Setsid: true`, `stdin` carrying the notice text, `stdout`/`stderr` appended to `.proc/<id>/hook.log`), marks the record `notified: true` in `meta.json`, and logs the wake to `.proc/<id>/hook.log`.
    - **Lock held (agent is mid-turn)**: **Skips rather than queuing** (`agent is mid-turn: wake skipped rather than queued, job left un-notified` in `.proc/<id>/hook.log`) and leaves `notified: false`. Queuing a second turn behind an active turn is the wrong behavior when the `end-of-turn` piggyback hook (or a manual read in the current turn) can surface the completion inside the turn already running.
- **Hook environment**: Scripts receive `WACKYPROC_AGENT_DIR`, `WACKYPROC_PROC_ID`, `WACKYPROC_TOOL`, `WACKYPROC_STATUS`, `WACKYPROC_EXIT_CODE`, `WACKYPROC_STDOUT_FILE`, `WACKYPROC_STDERR_FILE`, `WACKYPROC_NOTICE_FILE`, and `WACKYPROC_BIN`.
- **Self-wake transport (P8)**: `10-wake-agent` strips `AGENT2AGENT` and `WACKYPUB_CALL_CHAIN` from the environment (so an inherited A2A call chain from the turn that spawned the job cannot cause `wackypub`'s cycle detector to reject the self-dispatch) and invokes `wackypub agent prompt <agent_id> "<notice>"` in the background. Because the wake is a fresh top-level turn targeting the agent's own ID (which must be listed in `<agentFolder>/WACKYPUB_ALLOWED_AGENTS`), it deliberately does **not** use `--async` or `WACKYPROC_SUPERVISED`.
- **What the woken turn sees (P9)**: The prompt text (`CompletionNotice`) names the job and exact retrieval commands:
  ```text
  [wackyproc] A background job you started finished, and nothing else was running, so this turn woke up to handle it.
    id:      <id>
    tool:    <tool>
    status:  COMPLETED
    exit:    0
    result:  wackyproc get <id>
    detail:  wackyproc describe <id>
  ```
- **Failure semantics (P5)**: Missing hook directory, failed lock probe, script error, or missing `wackypub` binary never blocks the supervisor or alters the job's exit status or captured output. Non-quiet skip reasons are recorded in `.proc/<id>/hook.log`.

### 3. End-of-turn piggyback (`end-of-turn/20-wackyproc-piggyback` & `wackyproc end-of-turn`)

Covers the completions that the idle wake skipped because the agent was mid-turn when the job finished.

Install:

```bash
cp examples/hooks/end-of-turn/20-wackyproc-piggyback <agentFolder>/hooks/end-of-turn/
chmod +x <agentFolder>/hooks/end-of-turn/20-wackyproc-piggyback
```

- **How it works**: When a `wackypub` turn reaches its end, `wackypub` invokes `20-wackyproc-piggyback` with `EndOfTurnPayload` JSON on stdin. The script delegates to `wackyproc end-of-turn` (`proc.EndOfTurnPiggyback`), which scans `.proc/` for terminal records with `notified: false` (`proc.ListPending`).
- **The `can_continue` gate**: Nine of `wackypub`'s ten `end-of-turn` fire sites are notify-only (`can_continue: false` — aborted turns, failure breaker, max tool turns, deferred images, compacted bail, or an exhausted D88 `maxAutoContinuations` budget) and discard any `continue` action. When `can_continue` is `false`, `wackyproc end-of-turn` returns `{}` and **leaves every pending record `notified: false`** so completions are preserved for the next turn rather than announced to a dropped continuation.
- **Continuation hand-off (P4, P7, P9)**: When `can_continue` is `true` and un-notified terminal jobs exist, `wackyproc end-of-turn` marks each surfaced record `notified: true` and outputs:
  ```json
  {
    "action": {
      "type": "continue",
      "reason": "wackyproc-piggyback",
      "output": "[wackyproc] N background job(s) you started finished while this turn was running, and this turn continues to handle them.\n\n  id:      <id>\n  tool:    <tool>\n  status:  COMPLETED\n  exit:    0\n  result:  wackyproc get <id>\n  detail:  wackyproc describe <id>\n..."
    }
  }
  ```
  `wackypub` appends this report as a `<CONTINUATION reason="wackyproc-piggyback">` user turn and runs one more turn iteration, bounded by D88's `MaxAutoContinuations`. With nothing pending (or if `wackyproc` is absent), the hook prints `{}` and the turn ends normally.

## Process-group isolation

Stop, crash detection, and liveness all operate on the process *group*, not the PID: `stop` signals the group so grandchildren die with the child, and a record is only `RUNNING` while its group is alive. This is what makes `stop` reliable against multi-process trees (`npx → node → server`) that bare-PID management would orphan.

## Install

Requires Go 1.21+ (no CGO). Install the latest release directly:

```bash
go install github.com/colinrgodsey/wackyproc@latest
```

The binary lands in `$(go env GOPATH)/bin/wackyproc` (usually `~/go/bin/wackyproc`); make sure that directory is on your `PATH`.

To pin to a specific checkout instead, build from the source tree:

```bash
go build -o bin/wackyproc .
```

Verify the install with a one-line smoke test:

```bash
wackyproc --help
```

## Build & Test

```bash
go build -o bin/wackyproc .
go test ./...
go vet ./...
```

## Origin and Security Testing

This tool is part of the [wackypub](https://github.com/colinrgodsey/wackypub) companion tool suite, vendored as a git submodule (`tools/wackyproc`). Architectural decisions are tracked in GitKB under `wackypub/decisions/wackyproc/` (starting with `d51-wackyproc-a-zero-daemon-background`), with the live 3-state security-testing checklist at wackypub's `.agents/SECURITY_TESTING.md`.

## License

MIT
