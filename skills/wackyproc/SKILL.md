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
Spawn the process in the background. `wackyproc` immediately returns an 8-character pronounceable slug process ID (e.g. `katoruvo`):
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

Note: `list --json` is COMPACT by contract - it returns only the table fields plus timestamps and NEVER includes command args (they can be multi-KB prompts). For full args use `wackyproc describe <proc_id>` / `describe --json`.
```

### 3. Wait for Background Jobs to Finish
Block up to `N` seconds for listed processes to reach a terminal state:
```bash
# First completed (default): returns the ID of the first listed process to finish
wackyproc wait 10 a1b2 c3d4

# Barrier: wait for ALL listed processes; returns the last one to finish
wackyproc wait 10 --all a1b2 c3d4
```
At least one process ID is required - the old bare any-mode wait is removed, and a wait with no IDs fails fast. A process already terminal at entry is returned immediately; a listed ID that does not exist fails immediately. A timeout exits non-zero without returning an ID. Requests longer than 500 seconds are silently capped at 500 - call `wait` again if nothing finished in time rather than requesting a single very long wait.

### 4. Retrieve Output (Stdout & Stderr)
Read the full captured stdout and stderr streams:
```bash
wackyproc get a1b2
```
`wackyproc get` streams output through standard stdout and stderr. In `wackypub`, large output is automatically captured into scratchpad entries. `get` is a pure stream: it marks nothing, and retrieving output has no effect on the record's lifetime.

### 5. Peek at Latest Output (Without Full Retrieval)
Check a long-running job's latest output cheaply without pulling a full dump:
```bash
wackyproc peek a1b2
# Or specify how many trailing lines to inspect (default 20):
wackyproc peek a1b2 --lines 50
```
`wackyproc peek` is a pure observer that reads trailing lines and writes no state. Use `peek` to monitor progress or check recent errors while a process is still running or before deciding to retrieve full output.

### 6. Terminate a Process Group
Gracefully stop a running process and all its child processes:
```bash
wackyproc stop a1b2
# Or specify a custom timeout (in seconds) before SIGKILL:
wackyproc stop a1b2 --timeout 5
```
Force-kill the process group immediately when `stop` is too gentle (wedged
process, hung model turn) or send an arbitrary signal by name or number:
```bash
wackyproc kill a1b2
wackyproc signal a1b2 USR1
wackyproc signal a1b2 15
```

### 7. Clean Up & Manage Process Records
`wackyproc run` automatically retires the oldest terminal records (by `Gen`) when the terminal count exceeds the cap (100), regardless of state; `list` self-heals the same cap before its snapshot.

- **Prune all finished jobs**: Manually dispose all terminal records:
  ```bash
  wackyproc prune
  ```
- **Force-dispose a stuck record**: `wackyproc remove a1b2` - works on RUNNING records that prune can never clear (the process died or hung outside the supervisor's capture window, so it has no exit and will never go terminal). The process is NOT signaled - stop or kill it first if you want it gone. The record's ID is recorded as disposed and never re-claimed.

---

## Supervised Execution & the WACKYPROC_SUPERVISED Attestation

Every tool process spawned by `wackyproc run` carries
`WACKYPROC_SUPERVISED=<supervisor-pid>` in its environment. The value is the
pid of the supervising process - it doubles as provenance ("wackyproc spawned
THIS process"). Downstream tools can gate on its presence to prove a call is
supervised (output captured and retrievable, not fire-and-forget); for
instance `wackypub agent prompt --async` refuses to run without it.

The supervised async pattern for an agent-to-agent dispatch:

```bash
wackyproc run wackypub agent <target> prompt --async "...NO_RESPONSE..."
```

The caller gets back an 8-character pronounceable slug process ID immediately, then polls with
`wackyproc list` / `wackyproc wait <id>` and retrieves the target's
model output from stdout with `wackyproc get <id>`. The NO_RESPONSE suffix is a
token-saver convention, not a guarantee: a model that ignores it just
produces normal output, which still lands in stdout where the caller can
read it.

**Hung processes are killed, not timed out.** There is deliberately no
deadline machinery: a hung supervised call holds the same locks and presents
the same risk profile as a hung normal call, so the remedy is identical -
name the process group you want gone and signal it:

```bash
wackyproc stop <id>      # SIGTERM, falls back to SIGKILL after the grace period
wackyproc kill <id>      # SIGKILL the whole process group immediately
wackyproc signal <id> USR1  # any other signal, by name or number
```


## Choosing between `wait` / `peek` / `list` / background-it

A long-running background process gives you four ways to follow it up, and
they are not interchangeable: `wait` blocks the turn, `peek` reads trailing
output without blocking, `list --json` is a cheap status snapshot, and
backgrounding just returns control and checks again next turn. Which one is
right depends on whether blocking is authorized, not on which is fastest.

**`wait` is a vetter's choice, not the agent's.** Before calling `wait`, you
must have either (a) explicit user/process consent to block the current turn
for N seconds, or (b) an autonomous loop where the user or parent process
has pre-authorized blocking. Without that consent, pick a non-blocking
alternative (`peek`, `list --json`) or return control and resume next turn.

**Default when unsure: ask.** If you cannot tell whether blocking is
appropriate, surface the choice to the user or parent process (for example
as a clarifying question) rather than guessing. A wrong guess either wedges
your own turn on a `wait 300` or makes the user wait for a status check they
did not ask for.

**Worked examples:**

- *"Run the build and let me know when it's done"* - the user is elsewhere.
  Background the build, then check `list --json` or `get` on a later turn
  and report completion; do **not** call `wait`.
- *"Run the build, I am waiting"* - the user is explicitly waiting.
  `wait N` is correct, with N chosen against the user's perceived tolerance.
- *Autonomous recovery loop* (for example retry-storm detection) - blocking
  is pre-authorized by the loop's design; `wait` is correct.
- *Progress visibility without blocking* - `peek` (trailing output) or
  `list --json` (status and exit codes). Neither consumes the record, and
  neither holds the turn.

**Operational rule of thumb:** any `wait` longer than ~30 seconds is a
parent-process decision - do not take it unilaterally. Short waits (<10s)
are usually fine to take without asking.

---

## Process Lifecycle States

| Status | Description |
| :--- | :--- |
| **`RUNNING`** | Target process and its supervisor are actively executing. |
| **`COMPLETED`** | Process exited normally with exit code `0`. |
| **`FAILED`** | Process exited with a non-zero exit code (e.g. `1`, `42`). |
| **`CRASHED`** | Process died abruptly (OOM killer, `SIGKILL`, host reboot) without recording an exit code. |

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
   wackyproc wait 30 <id>
   wackyproc get <id>
   ```
3. **Clean Up Finished Jobs**:
   Run `wackyproc prune` to clean up all terminal processes on demand. `wackyproc run` will also automatically retire the oldest terminal processes (by `Gen`) once the cap of 100 terminal records is exceeded, regardless of state.
