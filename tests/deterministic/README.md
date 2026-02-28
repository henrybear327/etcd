# Deterministic Simulation Testing

This package implements deterministic simulation testing for etcd using
[gosim](https://github.com/jellevandenhooff/gosim). It runs a 3-node etcd
cluster inside gosim's simulated environment, exercises the cluster with
traffic and stop/restart events, and validates correctness using porcupine
linearizability checking and watch guarantee validation.

## Prerequisites

- Go 1.26+
- gosim binary: `cd /path/to/gosim && go build -o gosim ./cmd/gosim && go install ./cmd/gosim`

## How to Run

```bash
cd tests
rm -rf ../.gosim/
export PATH="$HOME/go/bin:$PATH"
GOWORK=off go test -run TestGosimPipeline -count=1 -v ./deterministic/
```

Run with a specific seed:
```bash
GOSIM_SEED=42 GOWORK=off go test -run TestGosimPipeline -count=1 -v ./deterministic/
```

Run all traffic profiles:
```bash
GOSIM_TRAFFIC_PROFILES=EtcdPutDeleteLease,EtcdPut,EtcdDelete \
GOWORK=off go test -run TestGosimPipeline -count=1 -v ./deterministic/
```

Run with higher operation count:
```bash
GOSIM_OPS_COUNT=200 GOWORK=off go test -run TestGosimPipeline -count=1 -v ./deterministic/
```

Full sweep (seeds × nemesis × profiles):
```bash
GOSIM_SEEDS=1,2,3 GOSIM_NEMESIS_TYPES=crash,none \
GOSIM_TRAFFIC_PROFILES=EtcdPutDeleteLease,EtcdPut,EtcdDelete \
GOWORK=off go test -run TestGosimPipeline -count=1 -v ./deterministic/
```

## Architecture

```
Phase 1: Simulation (//go:build sim)     Phase 2: Validation (//go:build !sim)
┌─────────────────────────────────┐     ┌─────────────────────────────────┐
│  gosim simulated environment    │     │  Normal Go test                 │
│                                 │     │                                 │
│  1. Start 3-node etcd cluster   │     │  1. metatesting.Run() → sim    │
│  2. Run weighted random traffic  │     │  2. Extract slog reports       │
│  3. Stop/restart nemesis        │     │  3. Construct ClientReports    │
│  4. Watch all key changes       │     │  4. Extract WAL → persisted    │
│  5. Stop cluster, read WAL      │     │  5. ValidateAndReturnVisualize │
│  6. Serialize via slog          │     │     - linearizability          │
│                                 │     │     - watch guarantees         │
│  NO validation in sim phase     │     │     - serializability          │
└─────────────────────────────────┘     └─────────────────────────────────┘
```

## Files

| File | Build Tag | Purpose |
|------|-----------|---------|
| `gosim_test.go` | `sim` | Test orchestrator: nemesis, watch, traffic dispatch |
| `traffic_test.go` | `sim` | Traffic profiles, keyStore, trafficClient, runTrafficOps |
| `cluster_test.go` | `sim` | testCluster, setupTestCluster, etcd node management |
| `recording_test.go` | `sim` | recordingClient, serialization, watch response conversion |
| `nemesis_test.go` | `sim` | verifyNodeServing |
| `wal_test.go` | `sim` | WAL reading inside gosim |
| `runner_test.go` | `!sim` | Metatesting pipeline: seeds × nemesis × profiles, validation |
| `DESIGN.md` | - | Design document with plan, challenges |
| `README.md` | - | This file |

## What It Tests

- **Linearizability**: All 10 operation types are linearizable (porcupine)
- **Watch guarantees**: 9 watch guarantees validated (ordering, completeness, etc.)
- **Serializable operations**: Stale reads are consistent
- **WAL persistence**: Persisted requests match observed operations
- **Stop/restart recovery**: Cluster recovers after node stop/restart
- **Degraded mode**: Cluster continues operating with 2/3 nodes
- **Traffic profiles**: EtcdPutDeleteLease (full), EtcdPut (read-heavy), EtcdDelete (delete stress)
- **Deterministic replay**: Same seed produces same results
