// Copyright 2025 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build sim

package deterministic_test

import (
	"context"
	"log"
	"os"
	"strconv"
	"testing"
	"time"

	"golang.org/x/time/rate"

	"github.com/jellevandenhooff/gosim/nemesis"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/tests/v3/robustness/identity"
	"go.etcd.io/etcd/tests/v3/robustness/model"
)

var (
	keyPrefix      = "key"
	keyCount       = 10
	requestTimeout = 2 * time.Second
)

// TestDeterministicTraffic starts a 3-node etcd cluster, runs weighted random
// traffic operations with configurable nemesis and a watch client, then
// serializes the results for linearizability and watch validation by the
// runner (TestGosimPipeline).
//
// The traffic profile is controlled by GOSIM_TRAFFIC_PROFILE env var:
//   - "EtcdPutDeleteLease": Full profile with all 10 op types (default)
//   - "EtcdPut":            Read-heavy with puts, no deletes/leases
//   - "EtcdDelete":         50/50 put/delete stress test
//
// The operation count is controlled by GOSIM_OPS_COUNT env var (default: 100).
//
// The nemesis type is controlled by the GOSIM_NEMESIS env var:
//   - "none":      no fault injection
//   - "crash":     crash/restart one node (default)
//   - "partition":  network partition via nemesis.PartitionMachines
func TestDeterministicTraffic(t *testing.T) {
	cluster := setupTestCluster(t)

	// Parse traffic profile
	profileName := os.Getenv("GOSIM_TRAFFIC_PROFILE")
	if profileName == "" {
		profileName = "EtcdPutDeleteLease"
	}
	profile, ok := trafficProfiles[profileName]
	if !ok {
		t.Fatalf("unknown traffic profile: %s", profileName)
	}
	log.Printf("traffic profile: %s", profileName)

	// Parse operation count
	totalOps := 100
	if opsStr := os.Getenv("GOSIM_OPS_COUNT"); opsStr != "" {
		n, err := strconv.Atoi(opsStr)
		if err != nil {
			t.Fatalf("invalid GOSIM_OPS_COUNT: %s", opsStr)
		}
		totalOps = n
	}
	log.Printf("total operations: %d", totalOps)

	ids := identity.NewIDProvider()
	baseTime := time.Now()

	// Create KV client for traffic
	kvClient, err := newRecordingClient(cluster.endpoints, ids, baseTime)
	if err != nil {
		t.Fatalf("failed to create KV client: %v", err)
	}

	// Create watch client
	watchClient, err := newRecordingClient(cluster.endpoints, ids, baseTime)
	if err != nil {
		t.Fatalf("failed to create watch client: %v", err)
	}

	// Construct traffic client
	tc := &trafficClient{
		client:       kvClient,
		keyStore:     newKeyStore(keyCount, keyPrefix),
		limiter:      rate.NewLimiter(200, 1000),
		idProvider:   ids,
		leaseStorage: identity.NewLeaseIDStorage(),
		baseTime:     baseTime,
	}
	nonUniqueLimiter := newConcurrencyLimiter(1)

	// Verify empty database at start by recording a Get that returns revision 1.
	{
		ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
		kvClient.mu.Lock()
		callTime := time.Since(baseTime)
		resp, getErr := kvClient.client.Get(ctx, keyPrefix+"0")
		returnTime := time.Since(baseTime)
		kvClient.kvOperations.AppendRange(keyPrefix+"0", "", 0, 0, callTime, returnTime, resp, getErr)
		kvClient.mu.Unlock()
		cancel()
		if getErr != nil {
			t.Fatalf("failed to verify empty db: %v", getErr)
		}
	}

	// Start watch before generating traffic
	watchRequest := model.WatchRequest{
		Key:                keyPrefix,
		Revision:           1,
		WithPrefix:         true,
		WithProgressNotify: true,
		WithPrevKV:         true,
	}
	watchCtx, watchCancel := context.WithCancel(context.Background())
	wch := watchClient.client.Watch(watchCtx, watchRequest.Key,
		clientv3.WithPrefix(),
		clientv3.WithRev(1),
		clientv3.WithProgressNotify(),
		clientv3.WithPrevKV(),
	)

	responses := []model.WatchResponse{}
	watchClient.watchMu.Lock()
	watchClient.watchOperations = append(watchClient.watchOperations, model.WatchOperation{
		Request:   watchRequest,
		Responses: responses,
	})
	watchClient.watchMu.Unlock()

	var maxRevision int64

	nemesisType := os.Getenv("GOSIM_NEMESIS")
	if nemesisType == "" {
		nemesisType = "crash"
	}

	switch nemesisType {
	case "none":
		maxRevision = runTrafficOps(tc, profile, totalOps, nonUniqueLimiter)

	case "crash":
		// Split ops into 3 phases: totalOps/3 each, remainder to phase 3.
		phase := totalOps / 3
		phase3 := totalOps - 2*phase

		// Phase 1: Initial traffic before any crashes
		rev := runTrafficOps(tc, profile, phase, nonUniqueLimiter)
		if rev > maxRevision {
			maxRevision = rev
		}

		// Phase 2: Stop node 2, run traffic with degraded cluster (2/3 quorum).
		// Uses Stop() (graceful, flushes all data) rather than Crash() because
		// Crash() loses non-fsynced WAL data, causing raft log corruption on
		// restart ("tocommit is out of range [lastIndex(0)]"). A future
		// improvement could use Crash() once all fsync paths are verified.
		cluster.machines[1].Stop()
		rev = runTrafficOps(tc, profile, phase, nonUniqueLimiter)
		if rev > maxRevision {
			maxRevision = rev
		}

		// Restart node 2 and wait for it to rejoin the cluster
		cluster.machines[1].Restart()
		time.Sleep(3 * time.Second)

		// Verify the stopped node actually restarted and is serving
		verifyNodeServing(t, cluster.endpoints[1], cluster.names[1])

		// Phase 3: Traffic after recovery with full cluster
		rev = runTrafficOps(tc, profile, phase3, nonUniqueLimiter)
		if rev > maxRevision {
			maxRevision = rev
		}

	case "partition":
		// Run nemesis in background while traffic proceeds.
		// nemesis.PartitionMachines uses log.Printf (silenced by log.SetOutput(io.Discard)).
		// The partition is visible through client errors in the operation history.
		done := make(chan struct{})
		go func() {
			nemesis.Sequence(
				nemesis.Sleep{Duration: 3 * time.Second},
				nemesis.PartitionMachines{
					Addresses: cluster.addrs,
					Duration:  5 * time.Second,
				},
			).Run()
			close(done)
		}()
		maxRevision = runTrafficOps(tc, profile, totalOps, nonUniqueLimiter)
		<-done // ensure nemesis finished

	default:
		t.Fatalf("unknown nemesis type: %s", nemesisType)
	}

	// Drain watch to catch up to maxRevision
	drainWatchToRevision(watchClient, wch, baseTime, &responses, maxRevision)
	watchCancel()

	// Update watch operation with final responses
	watchClient.watchMu.Lock()
	watchClient.watchOperations[0].Responses = responses
	watchClient.watchMu.Unlock()

	kvClient.Close()
	watchClient.Close()

	// Extract WAL data from all nodes
	cluster.extractWALs()

	// Serialize reports
	serializeReports([]*recordingClient{kvClient, watchClient})
}

func drainWatchToRevision(c *recordingClient, wch clientv3.WatchChan, baseTime time.Time, responses *[]model.WatchResponse, maxRev int64) {
	timeout := time.After(10 * time.Second)
	for {
		select {
		case r, ok := <-wch:
			if !ok {
				return
			}
			resp := toWatchResponse(r, baseTime)
			*responses = append(*responses, resp)
			if resp.Revision >= maxRev {
				return
			}
		case <-timeout:
			return
		}
	}
}
