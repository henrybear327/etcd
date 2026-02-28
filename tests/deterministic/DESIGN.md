# Deterministic Simulation Testing — Design Document

## Plan

| Step | Description | Status |
|------|-------------|--------|
| E1 | Branch setup + dependencies + documentation | Done |
| E2 | Core infrastructure (helpers, gosim_test, runner) | Done |
| E3 | Watch validation | Done |
| E4 | WAL extraction for persisted requests | Done |
| E5 | Crash/restart + network partition nemesis | Done |
| E6 | Final documentation and cleanup | Done |

## Architecture

### Two-Phase Design

```
┌─────────────────────────────────────────────────────────────┐
│ Phase 1: Simulation (//go:build sim — runs inside gosim)    │
│                                                              │
│  1. Start 3-node etcd cluster                               │
│  2. Run weighted random traffic (configurable profile)      │
│  3. Run configurable nemesis (crash/partition/none)         │
│  4. Watch all key changes from revision 1                   │
│  5. Drain watches to catch up to latest revision            │
│  6. Stop cluster, read WAL from each node                   │
│  7. Serialize ALL collected data via slog at the end:       │
│     - slog.Info("gosim_report", ...) — all client ops+watch │
│     - slog.Info("gosim_wal_report", ...) — per-node WAL     │
│                                                              │
│  ⚠ NO validation happens here — only data collection!       │
│                                                              │
├─────────────────────────────────────────────────────────────┤
│ Phase 2: Validation (//go:build !sim — normal Go test)      │
│                                                              │
│  1. metatesting.ForCurrentPackage(t).Run() → runs Phase 1  │
│     (iterates over seeds × nemesis × traffic profiles)     │
│  2. metatesting.ParseLog() → extract all slog entries       │
│  3. MustFindLogValue("gosim_report") → deserialize client   │
│     ops & watches → construct []report.ClientReport         │
│  4. MustFindLogValue("gosim_wal_report") → deserialize WAL  │
│     entries → MergeMembersEntries → []model.EtcdRequest     │
│  5. ValidateAndReturnVisualize(reports, persistedRequests)   │
│     → runs porcupine linearizability check                  │
│     → runs watch guarantee validation (9 guarantees)        │
│     → runs serializable operation validation                │
│                                                              │
│  ✓ All validation happens HERE, ONCE, with COMPLETE data    │
│                                                              │
│  ExtraEnv passes GOSIM_NEMESIS, GOSIM_TRAFFIC_PROFILE,      │
│  GOSIM_OPS_COUNT, and ETCD_LOG_LEVEL into sim               │
└─────────────────────────────────────────────────────────────┘
```

### Client Topology

```
2 recording clients:

KV Traffic:
  1 × cluster client (round-robin across all 3 members)

Watches:
  1 × watch client (WithPrefix + WithProgressNotify + WithPrevKV)
```

### Data Flow

```
sim phase:
  recordingClient → AppendableHistory → serializeReports() → slog.Info("gosim_report")
  Machine.Stop() → Restart(WAL reader) → readAndReportWAL() → slog.Info("gosim_wal_report")

runner phase:
  slog entries → JSON parse → []report.ClientReport → ValidateAndReturnVisualize
  slog entries → JSON parse → WAL merge (report.MergeMembersEntries) → []model.EtcdRequest
```

### Nemesis Scenarios

The nemesis type is controlled by `GOSIM_NEMESIS` env var (passed via `ExtraEnv`):

| Type | Description | Traffic Pattern |
|------|-------------|-----------------|
| `none` | No fault injection | All ops without fault injection |
| `crash` (default) | Crash/restart one node | 3-phase: ops/3 → crash → ops/3 → restart+verify → remainder |
| `partition` | Network partition via `nemesis.PartitionMachines` | All ops with background partition (3s delay, 5s duration) |

The crash scenario uses explicit 3-phase interleaving for precise control.
The partition scenario runs `nemesis.PartitionMachines` in a background
goroutine while traffic proceeds — the partition randomly splits the 3
addresses into two groups and disables connectivity between them.

### Traffic Engine

The traffic engine uses weighted random operation selection matching the
Antithesis robustness test profiles. Three profiles are available:

| Profile | Description | Key Operations |
|---------|-------------|----------------|
| `EtcdPutDeleteLease` (default) | Full profile with all 10 op types | Put, Get, Delete, Lease, MultiOpTxn, CompareAndSet |
| `EtcdPut` | Read-heavy with puts | Put (40%), Get, List, StaleGet, StaleList, MultiOpTxn |
| `EtcdDelete` | Delete stress test | Put (50%), Delete (50%) |

```
┌─────────────────────────────────────────────────────────────┐
│ Traffic Engine Architecture                                  │
│                                                              │
│  trafficClient                                               │
│  ├── recordingClient (KV ops + AppendableHistory)            │
│  ├── keyStore (10 keys, prefix-based, syncs on List)         │
│  ├── rate.Limiter (200 rps, burst 1000)                      │
│  ├── identity.Provider (unique request IDs)                  │
│  └── identity.LeaseIDStorage (per-client lease tracking)     │
│                                                              │
│  runTrafficOps(tc, profile, count, limiter)                  │
│  ├── pickRandom(profile) → weighted op selection             │
│  ├── concurrencyLimiter → filters non-unique writes on fail  │
│  ├── executeRequest() → dispatches to 10 operation types     │
│  └── returns maxRevision for watch drain                     │
└─────────────────────────────────────────────────────────────┘
```

Each operation is recorded via `AppendableHistory` for linearizability
validation. Failed operations trigger a `List` on the next iteration to
resync the key store. Non-unique writes (Delete, LeaseRevoke) are gated
by a `concurrencyLimiter` and filtered out when the limiter is exhausted.

### Traffic Pattern (crash nemesis)

```
Phase 1 (ops/3):     Normal traffic, all 3 nodes healthy
  ↓ Stop node 2
Phase 2 (ops/3):     Degraded traffic, 2/3 quorum
  ↓ Restart node 2, wait 3s for rejoin, verify serving
Phase 3 (remainder): Traffic after recovery, full cluster
```

Operation count is configurable via `GOSIM_OPS_COUNT` (default: 100).

### WAL Extraction

Signal handling is a no-op in gosim, so `etcdMain` blocks forever and
WAL reading code after it is never reached. The solution:

1. After traffic completes, call `Machine.Stop()` on each machine
2. Set a new `MainFunc` that only reads and reports WAL data
3. Call `Machine.Restart()` — disk state is preserved across stop/restart
4. Call `Machine.Wait()` to wait for the WAL reader to complete

## Debug Logging

### ETCD_LOG_LEVEL

The `ETCD_LOG_LEVEL` env var controls etcd server log verbosity inside the
simulation. It is the gosim equivalent of `EXPECT_DEBUG` in e2e tests.

**Important**: This uses `ETCD_LOG_LEVEL` (not `GOSIM_LOG_LEVEL`) to avoid
interfering with gosim's slog handler. Gosim reads `GOSIM_LOG_LEVEL` internally
to set the slog default handler level. If `GOSIM_LOG_LEVEL=warn`, slog filters
out INFO-level messages — including our `slog.Info("gosim_report", ...)` and
`slog.Info("gosim_wal_report", ...)` calls, causing validation to panic.

```bash
# Normal run (default: EtcdPutDeleteLease, 100 ops, crash nemesis):
GOWORK=off go test -run TestGosimPipeline -count=1 -v ./deterministic/

# All 3 profiles × 2 nemesis types:
GOSIM_TRAFFIC_PROFILES=EtcdPutDeleteLease,EtcdPut,EtcdDelete \
GOSIM_NEMESIS_TYPES=none,crash \
GOWORK=off go test -run TestGosimPipeline -count=1 -v ./deterministic/

# Higher ops count:
GOSIM_OPS_COUNT=200 GOWORK=off go test -run TestGosimPipeline -count=1 -v ./deterministic/

# Debug a failure (bounded memory at warn/error level):
ETCD_LOG_LEVEL=warn GOSIM_SEED=42 GOWORK=off go test -run TestGosimPipeline -count=1 -v ./deterministic/

# Full debug (high memory, short tests only):
ETCD_LOG_LEVEL=info GOSIM_SEED=42 GOWORK=off go test -run TestGosimPipeline -count=1 -v ./deterministic/
```

The env var is forwarded from the runner side to the sim side via gosim's
`ExtraEnv` mechanism. Inside the simulation, `makeZapLogger()` reads it
and creates a zap logger at the specified level (or `zap.NewNop()` when unset).

`log.SetOutput(io.Discard)` for the standard library `log` package remains
unconditional regardless of `ETCD_LOG_LEVEL`, because gRPC and raft produce
extremely high-volume output through `log.Printf` with no level-based filtering.

### Why EXPECT_DEBUG Doesn't Apply

`EXPECT_DEBUG` is defined in `pkg/expect/expect.go`. It controls debug output
for `ExpectProcess`, which wraps `os/exec.Command` to spawn external OS
processes. In gosim, etcd runs in-process via `embed.StartEtcd()`. There is
no process spawning, no `os/exec.Command`, no `ExpectProcess`. The `pkg/expect`
package is never imported.

| Aspect | e2e tests (EXPECT_DEBUG) | gosim tests |
|--------|--------------------------|-------------|
| How etcd runs | Separate OS process | In-process via `embed.StartEtcd()` |
| How logs captured | Read from process stdout | Buffered in gosim's memory |
| Debug toggle | `EXPECT_DEBUG=true` | `ETCD_LOG_LEVEL=warn` via ExtraEnv |
| What it controls | Print process output to test stdout | Switch `zap.NewNop()` to real logger |
| Memory concern | None (streaming) | Real (all output buffered) |

## Running Multiple Seeds

The runner supports parameterized execution over seeds, nemesis types, and
traffic profiles:

```bash
# Single seed (default):
GOWORK=off go test -run TestGosimPipeline -count=1 -v ./deterministic/

# Multiple seeds:
GOSIM_SEEDS=1,2,3,4,5 GOWORK=off go test -run TestGosimPipeline -count=1 -v ./deterministic/

# Multiple nemesis types:
GOSIM_NEMESIS_TYPES=crash,partition,none GOWORK=off go test -run TestGosimPipeline -count=1 -v ./deterministic/

# Multiple traffic profiles:
GOSIM_TRAFFIC_PROFILES=EtcdPutDeleteLease,EtcdPut,EtcdDelete \
GOWORK=off go test -run TestGosimPipeline -count=1 -v ./deterministic/

# Full nightly sweep (seeds × nemesis × profiles):
GOSIM_SEEDS=1,2,3,4,5 GOSIM_NEMESIS_TYPES=crash,none \
GOSIM_TRAFFIC_PROFILES=EtcdPutDeleteLease,EtcdPut,EtcdDelete \
GOWORK=off go test -run TestGosimPipeline -count=1 -v ./deterministic/

# Debug a specific failing seed:
ETCD_LOG_LEVEL=warn GOSIM_SEED=42 GOWORK=off go test -run TestGosimPipeline -count=1 -v ./deterministic/
```

Each combination runs as a subtest: `TestGosimPipeline/seed1/crash/EtcdPutDeleteLease`,
`TestGosimPipeline/seed1/partition/EtcdPut`, etc.

Environment variables:
- `GOSIM_SEEDS`: Comma-separated list of seeds (e.g. `"1,2,3,42"`)
- `GOSIM_SEED`: Single seed (fallback if `GOSIM_SEEDS` is not set)
- `GOSIM_NEMESIS_TYPES`: Comma-separated nemesis types (e.g. `"crash,partition,none"`)
- `GOSIM_TRAFFIC_PROFILES`: Comma-separated profile names (e.g. `"EtcdPutDeleteLease,EtcdPut,EtcdDelete"`)
- `GOSIM_OPS_COUNT`: Number of operations per test run (default: 100)
- `ETCD_LOG_LEVEL`: Log level for etcd server inside simulation
- `GOWORK=off`: Required to use the correct go.mod (not workspace)

## File Organization

```
tests/deterministic/
    cluster_test.go      -- testCluster, setupTestCluster, makeZapLogger, parseUrls, runEtcdNode, etcdMain
    recording_test.go    -- recordingClient, newRecordingClient, toWatchResponse, serialization types, serializeReports
    nemesis_test.go      -- verifyNodeServing
    gosim_test.go        -- TestDeterministicTraffic, drainWatchToRevision
    traffic_test.go      -- Traffic profiles (etcdPutDeleteLease/Put/Delete), keyStore, trafficClient, runTrafficOps
    wal_test.go          -- readAndReportWAL, readAllWALEntries (sim-side WAL reading)
    runner_test.go       -- TestGosimPipeline (parameterized runner), validateResult, extractPersistedRequests
    DESIGN.md            -- This document
    README.md            -- Quick start guide
```

All sim-side files use `//go:build sim`. The runner uses `//go:build !sim`.

## Implementation Progress

- [x] E1: gitignore, gosim dependency, documentation skeleton
- [x] E2: recordingClient, cluster setup, simple put/get, metatesting pipeline
- [x] E3: Watch client with progress notify and prev KV
- [x] E4: WAL reading via machine stop/restart, persisted requests validation
- [x] E5: Crash/restart nemesis + network partition nemesis
- [x] E6: Documentation update, file organization refactor

## Challenges

### 1. Simulated Time and Timestamp Ordering

**Problem**: Gosim's time model returns identical values for consecutive
`time.Now()` calls within the same scheduler step. This violates porcupine's
`AppendableHistory` constraints:
- `op.Call < op.Return`
- `op_n+1.Call > op_n.Call`
- `op_n+1.Call > op_n.Return`

**v2 Approach**: Added `ensureReturnAfterCall()`, `monotonicCallTime()`, and
`nextMinCallTime` hacks throughout the recording client and traffic code.
This worked but was fragile, required 14+ call sites to be modified, and
obscured the actual time semantics.

**v3 Approach**: Added `nanotimeStep` feature to gosim itself. With
`gosimruntime.SetNanotimeStep(1)`, every `time.Now()` call advances the
simulated clock by 1ns, guaranteeing unique timestamps naturally. Zero
application-level hacks needed.

**Lesson**: Fix time ordering at the simulator level rather than hacking
around it in application code.

### 2. Metatesting Memory Pressure

**Problem**: Gosim's metatesting framework captures ALL log output in an
in-memory `bytes.Buffer` (`gosimruntime/runtime.go` lines 357-361). The
etcd server, gRPC, raft, and standard library generate massive amounts of
log output. With concurrent traffic (14 clients), memory usage exceeded
9.7GB and the simulation never completed.

**Why it's expensive — technical details**: Gosim simulates goroutines
using Go coroutines (`runtime.newcoro`). The scheduler processes goroutines
one at a time. Each scheduling step involves:
1. `fastrandn(N)` — pick random goroutine
2. `checksummer.recordIntInt(runPick)` — FNV-1a hash for determinism
3. `globalPtr.set(pick.machine.globals)` — set thread-local globals
4. `pick.coro.Next()` — coroutine context switch (expensive)
5. `race.Acquire()` / `race.Release()` — race detector sync on every yield
6. `checksummer.recordIntInt(runResult)` — hash result for determinism
7. Scheduler list mutations — add/remove from runnable/goroutines lists

An etcd 3-node cluster spawns hundreds of goroutines (raft, gRPC, watchers,
leases) before any client traffic begins. Each additional client adds more.
The total goroutine count determines step count, and each step has high
constant-factor overhead. This is a fundamental property of deterministic
simulation — not a bug. See Gosim Design Limitations L6 and L7.

**Resolution**: Three-pronged logging suppression:
1. `makeZapLogger()` returns `zap.NewNop()` by default (configurable via
   `GOSIM_LOG_LEVEL`) — silences etcd server logs
2. `log.SetOutput(io.Discard)` — silences standard library log
3. Skip `SetupGlobalLoggers()`, use `zap.ReplaceGlobals()` directly —
   prevents gRPC from logging to stderr (captured by gosim)

Report data uses `slog.Info()` directly, which is separate from the
suppressed log output.

### 3. Recording Client Cannot Import robustness/client

**Problem**: The real `RecordingClient` in `robustness/client/` imports
`framework/e2e` which has dependencies incompatible with gosim's translated
environment.

**Resolution**: Re-implemented `recordingClient` in `recording_test.go`
using `model.AppendableHistory` directly. Same semantic behavior, different
implementation that avoids the problematic imports.

### 4. WAL Reading Inside Gosim

**Problem**: `report/wal.go` functions import packages incompatible with
gosim (transitively via `framework/e2e`).

**Resolution**: The sim-side WAL reading (`wal_test.go`) re-implements the
WAL entry reading logic using only sim-compatible imports. The runner side
(`runner_test.go`) uses the now-exported `report.MergeMembersEntries` and
`report.ParseEntryNormal` functions — no more duplicated parsing code.

### 5. Signal Handling Is a No-Op in Gosim

**Problem**: `osutil.HandleInterrupts` sets up signal handlers, but gosim
stubs out all signal functions (`OsSignal_signal_recv()` blocks forever).
This means etcd's main loop never exits, and post-shutdown code (like WAL
reading) is never reached.

**Resolution**: Use `Machine.Stop()` to terminate the machine externally,
then `Machine.Restart()` with a new `MainFunc` that reads WAL data. The
disk state is preserved across stop/restart.

Also replaced `lg.Fatal()` (which calls `os.Exit`, intercepted as panic
in gosim) with `slog.Error()` + return for recoverable errors like
startup failures during restart.

### 6. Cluster State on Restart

**Problem**: After stopping and restarting a node, etcd fails with "member
has already been bootstrapped" because `ClusterState` is set to "new".

**Resolution**: Check if the data directory exists on startup. If it does,
use `ClusterStateFlagExisting` instead of `ClusterStateFlagNew`. When WAL
exists, etcd uses `bootstrapClusterWithWAL` regardless of the flag, but
the filesystem check prevents the "new cluster with existing member" error.

### 7. Crash vs Stop for Nemesis

**Problem**: `Machine.Crash()` does NOT flush non-fsynced writes to disk.
After crash, the etcd data directory (including WAL) was completely
missing, preventing the node from restarting.

**Root Cause**: `TouchDirAll` (`client/pkg/fileutil/fileutil.go`) uses
`os.MkdirAll` to create directories but does NOT fsync the parent
directory afterward. On POSIX-compliant filesystems, a new directory
entry is only persisted when the **parent** directory is fsynced. gosim
faithfully models this behavior — on real ext4 with journaling, metadata
journal commits paper over this gap within ~5 seconds, but the bug is
real.

The fsync chain analysis:
```
TouchDirAll("etcd-2.etcd")         → MkdirAll → NO parent fsync → entry NOT persisted
TouchDirAll("etcd-2.etcd/member")  → MkdirAll → NO parent fsync → entry NOT persisted
wal.Create()                       → fsyncs member/ dir → "wal/" IS persisted
wal.Create()                       → fsyncs WAL file data
After crash: WAL data ON DISK but directory path MISSING
```

**Resolution**: Added `SyncDir` helper and modified `TouchDirAll` to
fsync the parent directory after `os.MkdirAll` creates a new directory.
This ensures directory entries survive crashes. The test verifies the
crashed node actually restarts and serves requests — a targeted Get to
the crashed node's endpoint catches restart failures that would
otherwise be masked by 2/3 quorum maintaining availability.

### 8. Empty Database Validation

**Problem**: `validateEmptyDatabaseAtStart()` requires at least one read
operation returning Revision 1 to confirm an empty database.

**Resolution**: Added an initial `Get(keyPrefix+"0")` before any puts,
which returns Revision 1 and is recorded in the operation history.

### 9. go.mod Replace Directive Mapping

**Problem**: `go.mod` replace directives (`go.etcd.io/etcd/client/pkg/v3
=> ../client/pkg`) resolve relative to the go.mod location. In a git
worktree, `../client/pkg` points to the worktree's copy, not the main
repo's. The `TouchDirAll` fix was applied to the main repo — the
worktree's copy was never modified.

**Resolution**: Before modifying any code referenced by a replace
directive, trace the physical path. In a worktree, `../` means the
worktree's parent, not the main repo's parent. See the Dependency
Boundary Map section for details.

## Gosim Design Limitations

This section documents design limitations in gosim discovered during etcd
integration, the root cause mechanism, and the workaround applied.

### L1. Identical Timestamps Within a Scheduler Step

**Mechanism**: Gosim's scheduler advances the simulated clock ONLY when
all goroutines are blocked and timers are waiting. Within a single
scheduler step, all `time.Now()` calls return the identical nanosecond
value. The clock jumps discretely to the next timer via `doadvance()`.

**Impact**: porcupine's `AppendableHistory` requires `op.Call < op.Return`
and strictly increasing call timestamps. Identical timestamps cause false
linearizability violations.

**Workaround**: `gosimruntime.SetNanotimeStep(1)` — each `time.Now()`
call advances the clock by 1ns, guaranteeing unique timestamps.

**Status**: Resolved permanently. Feature merged into gosim.

### L2. Signals Block Forever

**Mechanism**: `os/signal` functions are stubbed in gosim.
`OsSignal_signal_recv()` uses `select {}` (infinite block). There is no
signal delivery mechanism.

**Impact**: etcd's `osutil.HandleInterrupts()` blocks forever. The main
loop never exits, so post-shutdown code is unreachable.

**Workaround**: Use `Machine.Stop()` / `Machine.Restart()` /
`Machine.SetMainFunc()` for external lifecycle control.

**Status**: By design. Not a bug — gosim intentionally doesn't simulate
OS signals.

### L3. os.Exit / log.Fatal Become Panics

**Mechanism**: The gosim runtime intercepts `os.Exit()` and converts it
to a panic. `log.Fatal()` calls `os.Exit(1)` internally, so it also
panics.

**Impact**: Any etcd startup error that calls `log.Fatal()` becomes a
panic, halting the machine without graceful cleanup.

**Workaround**: Replace `lg.Fatal()` with `slog.Error()` + `return` for
recoverable errors (e.g., startup failures during restart after crash).

**Status**: Requires per-call-site fixes in etcd code that runs under
gosim.

### L4. Virtual Filesystem Faithfully Models POSIX Fsync Semantics

**Mechanism**: gosim's virtual filesystem correctly implements POSIX: a
directory entry is persisted ONLY when the parent directory is fsynced.
`Sync(inode)` flushes only that inode's pending operations.
`CrashClone()` with `graceful=false` keeps only previously-fsynced state.

**Impact**: `TouchDirAll` uses `os.MkdirAll` without fsyncing the parent
directory. After `Machine.Crash()`, the entire directory tree was missing.
On real ext4 with journal mode, metadata commits paper over this within
~5 seconds, masking the bug.

**Workaround**: Added `SyncDir()` helper and modified `TouchDirAll()` to
fsync the parent directory after `os.MkdirAll()`. This is the correct
POSIX fix and benefits real deployments too.

**Status**: Resolved. This was a real etcd bug exposed by gosim's correct
fsync modeling.

### L5. Machine.Crash() vs Machine.Stop() — Different Disk State

**Mechanism**: `Stop()` calls `FlushEverything()` which copies in-memory
state to persisted state. `Crash()` does NOT flush — only previously-
fsynced data survives. `RestartWithPartialDisk()` randomly applies some
non-fsynced writes (more realistic than either extreme).

**Impact**: Must choose the right method for each use case:
- Crash testing → `Machine.Crash()` (realistic, may lose data)
- WAL extraction → `Machine.Stop()` (preserves all data)
- Realistic crash with some writes → `Machine.RestartWithPartialDisk()`

**Workaround**: Use `Stop()` for WAL extraction, `Crash()` for nemesis
testing.

**Status**: By design. The three methods serve different testing purposes.

### L6. Unbounded Log Buffer in Metatesting

**Mechanism**: `run()` in gosimruntime allocates a `bytes.Buffer` with
`captureLog=true` (always set by metatesting). Every log message from
every goroutine appends to this buffer. The buffer is never flushed
during execution.

**Impact**: An etcd cluster generates massive log volume. With concurrent
traffic, the buffer can exceed 9.7GB.

**Workaround**: Triple logging suppression (see Challenge 2). Report data
passes through `slog.Info()`, which is separate from the suppressed output.

**Status**: Requires logging suppression in every gosim test. Cannot be
fixed without gosim adding bounded log capture or streaming.

### L7. Single-Threaded Deterministic Scheduler

**Mechanism**: The scheduler processes one goroutine per step,
sequentially. It randomly picks from the runnable list, context-switches
via `coro.Next()`, records checksums for determinism, and manages
scheduler lists. There is no parallelism across machines.

**Impact**: More goroutines = more sequential steps = linear increase in
CPU time. Each step has high constant-factor overhead: coroutine context
switch, race detector acquire/release, FNV-1a checksumming.

**Workaround**: None — this is fundamental to deterministic simulation.
Minimize goroutine count where possible (logging suppression reduces
log-processing goroutines), but do not sacrifice test fidelity.

**Status**: By design. Determinism requires single-threaded scheduling.

## Dependency Boundary Map

Before writing any code for gosim tests, trace the import graph. Packages
that transitively import `framework/e2e`, `os/exec`, or other
gosim-incompatible dependencies cannot be used inside the `//go:build sim`
boundary.

### Sim-Compatible Packages (can import in `//go:build sim`)

| Package | Why |
|---------|-----|
| `go.etcd.io/etcd/tests/v3/robustness/model` | Pure data types, no I/O |
| `go.etcd.io/etcd/tests/v3/robustness/identity` | ID generation, no external deps |
| `go.etcd.io/etcd/client/v3` (clientv3) | gRPC client, works in gosim |
| `go.etcd.io/etcd/server/v3/embed` | Embeds etcd server, the whole point |
| `go.etcd.io/etcd/server/v3/storage/wal` | WAL reading, pure I/O |
| `go.etcd.io/etcd/server/v3/storage/datadir` | Path helpers |
| `go.etcd.io/etcd/client/pkg/v3/fileutil` | File utilities |
| `go.etcd.io/raft/v3/raftpb` | Protobuf types |

### Sim-Incompatible Packages (CANNOT import in `//go:build sim`)

| Package | Why |
|---------|-----|
| `go.etcd.io/etcd/tests/v3/robustness/client` | Imports `framework/e2e` |
| `go.etcd.io/etcd/tests/v3/robustness/report` | Imports `framework/e2e` via same package |
| `go.etcd.io/etcd/tests/v3/framework/e2e` | Uses `os/exec`, `pkg/expect` |
| `go.etcd.io/etcd/pkg/v3/expect` | Uses `os/exec.Command` |

### Consequence

- `recordingClient` must be re-implemented locally (cannot reuse `robustness/client`)
- WAL entry reading must be copied (cannot import `robustness/report` inside sim)
- The runner side (`//go:build !sim`) CAN import `robustness/report` — it runs in
  normal Go, not inside gosim
- WAL parsing functions (`MergeMembersEntries`, `ParseEntryNormal`) are now
  exported from `report/wal.go` and used by the runner side directly

### go.mod Replace Directives

In a git worktree, `replace` directives resolve relative to the go.mod
location:
```
replace go.etcd.io/etcd/client/pkg/v3 => ../client/pkg
```
`../client/pkg` points to the **worktree's** copy, not the main repo's
copy. Before modifying any code referenced by a replace directive, trace
the physical path.

## Lessons Learned

Retrospective analysis of what we learned during the gosim integration
and what we would do differently starting from scratch.

### Lesson 1: Fix abstractions at the right layer

gosim returned identical `time.Now()` within the same scheduler step,
violating porcupine's timestamp ordering. v2 added 14+ application-level
hacks (`ensureReturnAfterCall`, `monotonicCallTime`, `nextMinCallTime`).
v3 added `gosimruntime.SetNanotimeStep(1)` — one line, zero hacks.

**Takeaway**: When a simulation tool provides the wrong abstraction,
extend the tool. If you write the same workaround in multiple places,
the fix belongs one layer down.

### Lesson 2: Understand gosim's goroutine simulation cost model

Metatesting buffers ALL output in memory. With 14 concurrent clients,
memory exceeded 9.7GB. Even with triple logging suppression, concurrent
goroutine simulation is expensive.

**Takeaway**: Computational cost is proportional to goroutine count. This
is fundamental to deterministic simulation, not a bug. See Gosim Design
Limitations L6 and L7 for full technical details.

### Lesson 3: Map dependency boundaries before writing code

`robustness/client/RecordingClient` imports `framework/e2e` → incompatible
with gosim. Same for WAL reading in `report/wal.go`.

**Takeaway**: Before writing any code, trace the import graph. See the
Dependency Boundary Map section for the full compatibility matrix.

### Lesson 4: Design around external lifecycle control

`osutil.HandleInterrupts` blocks forever in gosim (signals are no-op).
`etcdMain` never exits. `lg.Fatal()` calls `os.Exit`, which panics in
gosim.

**Takeaway**: Use `Machine.Stop()/Crash()/Restart()/SetMainFunc()` for
lifecycle control. Replace `os.Exit`/`log.Fatal` paths with
`slog.Error()` + return.

### Lesson 5: Verify crash recovery at the individual node level

After `Machine.Crash()`, etcd data directories were missing (unfsynced
`MkdirAll`). Tests "passed" because 2/3 quorum maintained availability
— the crashed node was permanently dead.

**Takeaway**: After every crash/restart, issue a direct request to the
restarted node's individual endpoint. A quorum-based system will silently
mask permanent node failures.

### Lesson 6: Know which copy of the code you're editing

`go.mod` replace directives resolve relative to the go.mod location. In
a git worktree, `../client/pkg` points to the worktree's copy, not the
main repo's.

**Takeaway**: Before modifying any code, trace each `replace` directive
to its physical directory. See the Dependency Boundary Map section.

### Lesson 7: Understand validation framework preconditions

`validateEmptyDatabaseAtStart()` requires at least one read returning
Revision 1. Without it, validation fails with a cryptic error.

**Takeaway**: Read `checkValidationAssumptions()` in
`validate/validate.go` before designing the test. The "empty database
probe" should be a standard initialization step.

### Lesson 8: The cluster state flag is dynamic, not static

After crash/restart, etcd fails with "member has already been
bootstrapped" because `ClusterState` was hardcoded to "new".

**Takeaway**: Configuration that normally comes from command-line flags
must be computed dynamically based on disk state when restart is possible.
Check `os.Stat(dataDir)` at startup.

## Architectural Improvements (v3 Redesign)

Changes made from the initial implementation based on lessons learned.

### Configurable debug logging via ExtraEnv

Replaced unconditional `zap.NewNop()` with `ETCD_LOG_LEVEL` env var
passed via gosim's `ExtraEnv`. Uses `ETCD_LOG_LEVEL` (not `GOSIM_LOG_LEVEL`)
to avoid interfering with gosim's slog handler, which reads
`GOSIM_LOG_LEVEL` and would suppress INFO-level report messages.
At `warn`/`error` level, memory is bounded. `log.SetOutput(io.Discard)`
remains unconditional because gRPC and raft produce high-volume output
through `log.Printf` with no level filtering.

### Nemesis package for composable fault injection

Added gosim's `nemesis` package for the network partition scenario:
`nemesis.PartitionMachines` randomly splits addresses into two groups and
disables connectivity. The crash scenario retains explicit 3-phase
interleaving for precise control and post-restart verification.

Note: `nemesis.RestartRandomly` uses `RestartWithPartialDisk()` which is
more realistic than `Restart()` — it flushes a random subset of
non-fsynced writes. Post-nemesis health checks are still needed since the
nemesis package doesn't verify recovery.

### Parameterized runner (seeds × nemesis × traffic profiles)

Runner iterates over seeds (`GOSIM_SEEDS`), nemesis types
(`GOSIM_NEMESIS_TYPES`), and traffic profiles (`GOSIM_TRAFFIC_PROFILES`),
turning a single test into a nightly sweep. Each combination runs as a
Go subtest for clear reporting.

### Test cluster abstraction

Extracted `testCluster` struct with `setupTestCluster()` and
`extractWALs()` methods, encapsulating gosim runtime configuration,
machine setup, and WAL extraction.

### Exported WAL parsing functions

Exported `MergeMembersEntries`, `ParseEntryNormal`, `ToEtcdOperation`
from `report/wal.go` (pure functions with no `framework/e2e` dependency),
eliminating duplicated code in `runner_test.go`. The sim-side copy in
`wal_test.go` must remain (cannot import `report/` inside gosim).

### File organization

Split the monolithic `helpers_test.go` into focused files:
`cluster_test.go`, `recording_test.go`, `wal_test.go`, `nemesis_test.go`.

## Future Work

- Concurrent clients when gosim supports lighter-weight goroutine simulation
- Custom nemesis patterns (leader targeting, rolling restarts)
- `nemesis.RestartRandomly` integration for crash testing
- Integration with CI/CD pipeline for nightly multi-seed sweeps
- Defragment operation (weight=0 in all Antithesis profiles, can be added later)
