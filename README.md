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

- `wackyproc run <tool> [args...]`: Spawns `./tools/<tool>` as a detached background process and outputs its 4-character ID. Stdin piped into `run` is drained into `.proc/<id>/stdin` before detaching.
- `wackyproc list [--json]`: Lists all tracked processes (`ID STATUS TOOL PID EXIT`) and their current status (`RUNNING`, `COMPLETED`, `FAILED`, `CRASHED`).
- `wackyproc wait [seconds]`: Blocks up to N seconds (default 500) until a process that was **still running when the call began** finishes. Processes already terminal at entry are never reported, and the call blocks to the timeout when there is nothing pending, exiting non-zero.
- `wackyproc wait --for <proc_id> [seconds]`: Blocks until that specific process finishes, reporting it immediately if it is already terminal. Fails immediately if the ID does not exist.
- `wackyproc get <proc_id>`: Dumps captured stdout and stderr to the terminal and marks terminal records as consumed (retrieval = consumption).
- `wackyproc peek <proc_id> [--lines N]`: Shows the trailing N lines (default 20) of captured stdout/stderr without a full dump, and never marks the record as consumed (unlike `get`).
- `wackyproc unconsume <proc_id>`: Clears the consumed sequence number of a process record, preserving it from auto-disposal.
- `wackyproc prune`: Disposes all terminal process records regardless of consumed state and reports removed IDs.
- `wackyproc stop <proc_id> [--timeout N]`: Gracefully stops the whole process group via `SIGTERM`, falling back to `SIGKILL` after N seconds (default 3).
- `wackyproc skill`: Prints the bundled agent skill guide (`skills/wackyproc/SKILL.md`, embedded in the binary).

## Consumption semantics and status taxonomy

Each record carries a consumed sequence number. `get` on a terminal record marks it consumed; `peek` never does; `unconsume` clears it. Terminal records are retained up to a cap (`MaxTerminalEntries = 100`, deliberately below the 300-entry scratchpad cap since records carry captured output) — `unconsume` preserves a record from auto-disposal, `prune` disposes terminal records regardless.

Status is derived from liveness plus exit code, never stored:

- `RUNNING` — PID alive in its recorded process group.
- `COMPLETED` — exited 0.
- `FAILED` — exited non-zero.
- `CRASHED` — no exit code recorded but the process is gone (kill -9, OOM, abrupt host crash; exit 137 from SIGKILL also lands here). Start-time validation guards against PID reuse masquerading as liveness.

## Process-group isolation

Stop, crash detection, and liveness all operate on the process *group*, not the PID: `stop` signals the group so grandchildren die with the child, and a record is only `RUNNING` while its group is alive. This is what makes `stop` reliable against multi-process trees (`npx → node → server`) that bare-PID management would orphan.

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
