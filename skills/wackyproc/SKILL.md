---
name: wackyproc
description: Guide for running and managing background processes across turns using wackyproc.
always_load: true
---
# WackyProc Process Management & Long-Running Command Guide

`wackyproc` is a zero-daemon background process manager companion tool designed for non-persistent AI agents. In turn-based runtimes like `wackypub`, an agent's process only lives for the duration of a single turn. `wackyproc` enables agents to spawn long-running tasks, detach them from the turn lifecycle, and monitor/retrieve their output across subsequent turns.

---

## When to Use `wackyproc` as a Proxy for `run_command`

Standard `run_command` executes tools synchronously and blocks the entire turn until execution finishes. If a tool takes too long or runs indefinitely, the turn blocks or times out.

Use `wackyproc run` instead of synchronous `run_command` for:
- **Builds & Compiles**: `cargo build`, `go build`, `npm run build`, `make`
- **Test Suites**: `pytest`, `go test ./...`, `npm test`
- **Servers & Background Daemons**: dev webservers, API mock servers, local proxy servers
- **Data Migrations & Heavy Scripts**: database migrations, bulk downloads, log processing

---

## Tool Resolution & Security Boundaries

`wackyproc` strictly resolves target tools against `./tools/<tool>` in the current working directory:
- `wackyproc run dev-server --port 8080` resolves to `./tools/dev-server`.
- **No `$PATH` fallback**: Running commands not present in `./tools/` (e.g. system `bash` or `curl` unless linked into `./tools/`) will be denied.

---

## Multi-Turn Agent Workflow

### 1. Launch a Background Process (Turn 1)
Spawn the process in the background. `wackyproc` immediately returns a 4-character process ID (e.g. `a1b2`):
```bash
wackyproc run build-tool --release
# Output: a1b2
```

To pass stdin to a background process (e.g. via scratchpad macros or piped data):
```bash
wackyproc run process-input < stdin_data
```
`wackyproc` drains the stdin synchronously before detaching so data is never lost across turns.

### 2. Inspect Running & Completed Jobs (Turn 2+)
List all tracked processes, their PIDs, exit statuses, and states:
```bash
wackyproc list
# ID    STATUS      TOOL        PID    EXIT
# a1b2  RUNNING     build-tool  41203  -

# Machine-readable JSON output:
wackyproc list --json
```

### 3. Wait for Background Jobs to Finish
Block up to `N` seconds for **any** tracked background process to complete:
```bash
wackyproc wait 10
# Output: a1b2
```
If a process finishes within 10 seconds, its ID is printed immediately. Requests longer than 500 seconds are silently capped at 500 - call `wait` again if nothing finished in time rather than requesting a single very long wait.

### 4. Retrieve Output (Stdout & Stderr)
Read the full captured stdout and stderr streams:
```bash
wackyproc get a1b2
```
`wackyproc get` streams output through standard stdout and stderr. In `wackypub`, large output is automatically captured into scratchpad entries.

### 5. Peek at Latest Output (Without Full Retrieval)
Check a long-running job's latest output cheaply without pulling a full dump:
```bash
wackyproc peek a1b2
# Or specify how many trailing lines to inspect (default 20):
wackyproc peek a1b2 --lines 50
```
`wackyproc peek` is a pure observer that reads the trailing lines and writes no state. Under future D79 consumption-order disposal, `get` will mark records as retrieved/consumed to make them eligible for disposal, whereas `peek` will never mark records consumed. Use `peek` to monitor progress or check recent errors while a process is still running or before deciding to retrieve full output.

### 6. Terminate a Process Group
Gracefully stop a running process and all its child processes:
```bash
wackyproc stop a1b2
# Or specify a custom timeout (in seconds) before SIGKILL:
wackyproc stop a1b2 --timeout 5
```

---

## Process Lifecycle States

| Status | Description |
| :--- | :--- |
| **`RUNNING`** | Target process and its supervisor are actively executing. |
| **`COMPLETED`** | Process exited normally with exit code `0`. |
| **`FAILED`** | Process exited with a non-zero exit code (e.g. `1`, `42`). |
| **`CRASHED`** | Process died abruptly (OOM killer, `SIGKILL`, host reboot) without recording an exit code (recorded as `137`). |

---

## Common Patterns & Best Practices

1. **Fire & Forget Server**:
   ```bash
   wackyproc run webserver --port 3000
   # Check if healthy on next turn:
   wackyproc list
   ```
2. **Compile -> Check -> Get Output**:
   ```bash
   # Turn 1:
   wackyproc run cargo-build --release
   # Turn 2:
   wackyproc wait 30
   wackyproc get <id>
   ```
3. **Clean Up Finished Jobs**:
   There is no `wackyproc` command for this yet - finished job logs and metadata remain on disk in `.proc/<id>/` indefinitely until removed by another means (e.g. `files-rw delete` or a shell tool, if linked). Unlike wackypub's own scratchpad system, `.proc/` has no automatic eviction, so a long-running workspace with many `wackyproc run` calls will accumulate state over time.
