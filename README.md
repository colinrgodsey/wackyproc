# wackyproc

A zero-daemon background process manager companion tool for turn-based, non-persistent AI agents.

In turn-based agent runtimes (like [wackypub](https://github.com/colinrgodsey/wackypub)), an agent process only lives for the duration of a single turn. `wackyproc` allows agents to spawn long-running background tasks (builds, test suites, servers) and check back on them across turns without requiring a master daemon or persistent background service.

## Core Design Principles

1. **Self-Supervising Detached Runner**: Spawns detached processes via `Setsid: true`, isolating the target child in its own process group (`Setpgid: true`).
2. **State on Disk (`.proc/<id>/`)**: Each process state is tracked in a dedicated directory containing `meta.json`, `pid`, `pgid`, `stdin`, `stdout`, `stderr`, and `exit_code`.
3. **Strict Tool Resolution**: Resolves commands strictly against `<cwd>/tools/<tool>` with **no `$PATH` fallback**, matching agent capability boundaries.
4. **Zero Scratchpad Format Coupling**: Dumps captured output via `wackyproc get <proc_id>`, allowing the host harness to handle auto-capture generically without coupling to internal formats.
5. **PID Reuse Protection**: Validates process start times against recorded metadata to detect PID recycling after abrupt crashes.

## Commands

- `wackyproc run <tool> [args...]`: Spawns `./tools/<tool>` as a detached background process and outputs its 4-character ID.
- `wackyproc list [--json]`: Lists all tracked processes and their current status (`RUNNING`, `COMPLETED`, `FAILED`, `CRASHED`).
- `wackyproc wait [seconds]`: Blocks up to N seconds (default 500) until a process that was **still running when the call began** finishes. Processes already terminal at entry are never reported, and the call blocks to the timeout when there is nothing pending, exiting non-zero.
- `wackyproc wait --for <proc_id> [seconds]`: Blocks until that specific process finishes, reporting it immediately if it is already terminal. Fails immediately if the ID does not exist.
- `wackyproc get <proc_id>`: Dumps captured stdout and stderr to the terminal.
- `wackyproc stop <proc_id> [--timeout N]`: Gracefully stops the process group via `SIGTERM`, falling back to `SIGKILL`.

## Build & Test

```bash
go build -o bin/wackyproc .
go test ./...
go vet ./...
```

## Origin and Security Testing

This tool is part of the [wackypub](https://github.com/colinrgodsey/wackypub) companion tool suite, vendored as a git submodule (`tools/wackyproc`). Architectural decisions and security testing guidelines are tracked in wackypub's [`.agents/DECISIONS.md`](https://github.com/colinrgodsey/wackypub/blob/main/.agents/DECISIONS.md) (D51) and [`.agents/SECURITY_TESTING.md`](https://github.com/colinrgodsey/wackypub/blob/main/.agents/SECURITY_TESTING.md).

## License

MIT
