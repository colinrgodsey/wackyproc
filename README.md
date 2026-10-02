# wackyproc

A zero-daemon background process manager companion tool for turn-based, non-persistent AI agents.

In turn-based agent runtimes (like [wackypub](https://github.com/colinrgodsey/wackypub)), an agent process only lives for the duration of a single turn. `wackyproc` allows agents to spawn long-running background tasks (builds, test suites, servers) and check back on them across turns without requiring a master daemon or persistent background service.

## Core Design Principles

1. **Self-Supervising Detached Runner**: Spawns detached processes via `Setsid: true`, isolating the target child in its own process group (`Setpgid: true`). No daemon to install, no socket to connect to — supervision is a re-exec of the same binary (`__supervise`, hidden).
2. **State on Disk (`.proc/<id>/`)**: Each process state is tracked in a dedicated directory containing `meta.json`, `pid`, `pgid`, `supervisor_pid`, `stdin`, `stdout`, `stderr`, and `exit_code`.
3. **Strict Tool Resolution**: Resolves commands strictly against `<cwd>/tools/<tool>` with **no `$PATH` fallback**, matching agent capability boundaries.
4. **Zero Scratchpad Format Coupling**: Dumps captured output via `wackyproc get <proc_id>`, allowing the host harness to handle auto-capture generically without coupling to internal formats.
5. **PID Reuse Protection**: Validates process start times against recorded metadata to detect PID recycling after abrupt crashes.

## Commands

- `wackyproc run <tool> [args...]`: Spawns `./tools/<tool>` as a detached background process and outputs its 8-character pronounceable slug ID (e.g. `katoruvo`). Stdin piped into `run` is drained into `.proc/<id>/stdin` before detaching.
- `wackyproc list [--json]`: Lists all tracked processes (`ID STATUS TOOL PID EXIT`) and their current status (`RUNNING`, `COMPLETED`, `FAILED`, `CRASHED`). `--json` is COMPACT by contract: it returns only the table fields plus timestamps (id, tool, status, pid, pgid, exit_code, started_at) and deliberately EXCLUDES command args - a dispatch's args can be multi-KB (agent prompts), and list is the status snapshot; use `describe` for full args.
- `wackyproc describe <proc_id> [more ids...] [--json]`: Full detail for one or more records - complete command args, cwd, tool path, output-file locations, consumed state. Does not mark consumed.
- `wackyproc wait [seconds]`: Blocks up to N seconds (default 500) until a process that was **still running when the call began** finishes. Processes already terminal at entry are never reported, and the call blocks to the timeout when there is nothing pending, exiting non-zero.
- `wackyproc wait --for <proc_id> [seconds]`: Blocks until that specific process finishes, reporting it immediately if it is already terminal. Fails immediately if the ID does not exist.
- `wackyproc get <proc_id>`: Dumps captured stdout and stderr to the terminal and marks terminal records as consumed (retrieval = consumption).
- `wackyproc peek <proc_id> [--lines N]`: Shows the trailing N lines (default 20) of captured stdout/stderr without a full dump, and never marks the record as consumed (unlike `get`).
- `wackyproc unconsume <proc_id>`: Clears the consumed sequence number of a process record, preserving it from auto-disposal.
- `wackyproc prune`: Disposes all terminal process records regardless of consumed state and reports removed IDs.
- `wackyproc remove <proc_id>`: Force-disposes a record regardless of status or consumed state - the escape hatch for stuck `RUNNING` records that `prune` can never clear (a process that died or hung outside the supervisor's capture window has no exit). The process is NOT signaled: if it is still alive it keeps running unmanaged, so stop or kill it first if you want it gone.
- `wackyproc stop <proc_id> [--timeout N]`: Gracefully stops the whole process group via `SIGTERM`, falling back to `SIGKILL` after N seconds (default 3).
- `wackyproc skill`: Prints the bundled agent skill guide (`skills/wackyproc/SKILL.md`, embedded in the binary).

## Consumption semantics and status taxonomy

Each record carries a consumed sequence number. `get` on a terminal record marks it consumed; `peek` never does; `unconsume` clears it. Terminal records are retained up to a cap (`MaxTerminalEntries = 100`, deliberately below the 300-entry scratchpad cap since records carry captured output). When the cap overflows, the oldest terminal records are auto-disposed (consumed-first, then unconsumed-oldest) with each disposal logged to stderr - the cap self-heals on every `list`/`run`; `unconsume` only DEPRIORITIZES a record (consumed records are evicted before unconsumed ones) — over the cap there is no retention guarantee, so `get` output promptly or `prune` deliberately. `prune` disposes terminal records regardless.
`remove` disposes a specific record in any state. Every disposal path records the ID in a disposed list, and ID generation never re-claims a disposed ID, so a fresh dispatch cannot inherit an ID that logs still reference from the previous record.

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
wackyproc wait --for <proc_id>
wackyproc get <proc_id>   # stdout holds the target's model output
```

There is no deadline machinery by design: a hung supervised process holds
the same locks and has the same risk profile as a hung normal call, so the
remedy is the existing kill signals - `wackyproc stop <id>` (SIGTERM, then
SIGKILL after the grace period) or `wackyproc kill <id>` (immediate
SIGKILL of the whole process group).

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
