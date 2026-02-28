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

//go:build !sim

package deterministic_test

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jellevandenhooff/gosim/metatesting"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"go.etcd.io/etcd/tests/v3/robustness/model"
	"go.etcd.io/etcd/tests/v3/robustness/report"
	"go.etcd.io/etcd/tests/v3/robustness/validate"
	"go.etcd.io/raft/v3/raftpb"
)

// operationRecord mirrors the type in helpers_test.go for JSON deserialization.
type operationRecord struct {
	ClientID int                     `json:"ClientId"`
	Input    model.EtcdRequest       `json:"Input"`
	Call     int64                   `json:"Call"`
	Output   model.MaybeEtcdResponse `json:"Output"`
	Return   int64                   `json:"Return"`
}

// clientReportData mirrors the type in helpers_test.go for JSON deserialization.
type clientReportData struct {
	ClientID   int                    `json:"client_id"`
	Operations []operationRecord      `json:"operations"`
	Watch      []model.WatchOperation `json:"watch"`
}

// gosimReport mirrors the type in helpers_test.go for JSON deserialization.
type gosimReport struct {
	Clients []clientReportData `json:"clients"`
}

// walEntryData mirrors the type in helpers_test.go for JSON deserialization.
type walEntryData struct {
	Data string `json:"data"`
}

// walReportData mirrors the type in helpers_test.go for JSON deserialization.
type walReportData struct {
	NodeName  string         `json:"node_name"`
	HardState string         `json:"hard_state"`
	Entries   []walEntryData `json:"entries"`
}

// getSeeds parses the GOSIM_SEEDS env var (e.g. "1,2,3,42") into a slice of
// seeds. Falls back to GOSIM_SEED for single-seed runs. Defaults to [1].
func getSeeds() []int64 {
	if seedsStr := os.Getenv("GOSIM_SEEDS"); seedsStr != "" {
		parts := strings.Split(seedsStr, ",")
		seeds := make([]int64, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			seed, err := strconv.ParseInt(p, 10, 64)
			if err != nil {
				continue
			}
			seeds = append(seeds, seed)
		}
		if len(seeds) > 0 {
			return seeds
		}
	}
	if seedStr := os.Getenv("GOSIM_SEED"); seedStr != "" {
		seed, err := strconv.ParseInt(seedStr, 10, 64)
		if err == nil {
			return []int64{seed}
		}
	}
	return []int64{1}
}

// getNemesisTypes parses the GOSIM_NEMESIS_TYPES env var (e.g. "crash,partition,none")
// into a slice of nemesis type strings. Defaults to ["crash"].
func getNemesisTypes() []string {
	if typesStr := os.Getenv("GOSIM_NEMESIS_TYPES"); typesStr != "" {
		parts := strings.Split(typesStr, ",")
		types := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				types = append(types, p)
			}
		}
		if len(types) > 0 {
			return types
		}
	}
	return []string{"crash"}
}

// getTrafficProfiles parses the GOSIM_TRAFFIC_PROFILES env var (e.g.
// "EtcdPutDeleteLease,EtcdPut,EtcdDelete") into a slice of profile names.
// Defaults to ["EtcdPutDeleteLease"].
func getTrafficProfiles() []string {
	if profilesStr := os.Getenv("GOSIM_TRAFFIC_PROFILES"); profilesStr != "" {
		parts := strings.Split(profilesStr, ",")
		profiles := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				profiles = append(profiles, p)
			}
		}
		if len(profiles) > 0 {
			return profiles
		}
	}
	return []string{"EtcdPutDeleteLease"}
}

func TestGosimPipeline(t *testing.T) {
	mt := metatesting.ForCurrentPackage(t)
	seeds := getSeeds()
	nemesisTypes := getNemesisTypes()
	trafficProfiles := getTrafficProfiles()

	for _, seed := range seeds {
		for _, nem := range nemesisTypes {
			for _, profile := range trafficProfiles {
				t.Run(fmt.Sprintf("seed%d/%s/%s", seed, nem, profile), func(t *testing.T) {
					extraEnv := []string{
						fmt.Sprintf("GOSIM_NEMESIS=%s", nem),
						fmt.Sprintf("GOSIM_TRAFFIC_PROFILE=%s", profile),
					}
					if lvl := os.Getenv("ETCD_LOG_LEVEL"); lvl != "" {
						extraEnv = append(extraEnv, "ETCD_LOG_LEVEL="+lvl)
					}
					if opsCount := os.Getenv("GOSIM_OPS_COUNT"); opsCount != "" {
						extraEnv = append(extraEnv, "GOSIM_OPS_COUNT="+opsCount)
					}

					t.Logf("Running gosim simulation with seed %d, nemesis %s, profile %s", seed, nem, profile)
					result, err := mt.Run(t, &metatesting.RunConfig{
						Test:     "TestDeterministicTraffic",
						Seed:     seed,
						ExtraEnv: extraEnv,
					})
					require.NoError(t, err, "failed to run gosim simulation")
					validateResult(t, result, seed)
				})
			}
		}
	}
}

// validateResult extracts reports from the simulation result and runs
// linearizability, watch, and serializable validation.
func validateResult(t *testing.T, result *metatesting.RunResult, seed int64) {
	t.Helper()

	if result.Failed {
		t.Fatalf("gosim simulation failed (seed=%d)", seed)
	}

	// Extract report from structured logs
	logs := metatesting.ParseLog(result.LogOutput)
	reportJSON := metatesting.MustFindLogValue(logs, "gosim_report", "report_json").(string)

	// Parse the report
	var simReport gosimReport
	require.NoError(t, json.Unmarshal([]byte(reportJSON), &simReport), "failed to parse gosim report")

	// Write NDJSON files for LoadClientReports
	reportDir := t.TempDir()
	writeClientReportFiles(t, reportDir, simReport)

	// Extract WAL reports
	persistedRequests := extractPersistedRequests(t, logs)

	// Load and validate
	lg, err := zap.NewProduction()
	require.NoError(t, err)

	reports, err := report.LoadClientReports(reportDir)
	require.NoError(t, err)

	t.Logf("Loaded %d client reports", len(reports))
	for _, r := range reports {
		t.Logf("  Client %d: %d KV operations, %d watch operations (%d watch events)",
			r.ClientID, len(r.KeyValue), len(r.Watch), r.WatchEventCount())
	}
	if persistedRequests != nil {
		t.Logf("Persisted requests: %d", len(persistedRequests))
	} else {
		t.Logf("Persisted requests: nil (WAL extraction failed)")
	}

	validateConfig := validate.Config{ExpectRevisionUnique: false}
	result2 := validate.ValidateAndReturnVisualize(
		lg, validateConfig, reports, persistedRequests, 5*time.Minute,
	)

	if result2.Assumptions.Error() != nil {
		t.Fatalf("assumption validation failed: %v", result2.Assumptions.Error())
	}

	if result2.Linearization.Timeout {
		t.Fatalf("linearization check timed out")
	}

	require.NoError(t, result2.Linearization.Error(), "linearization validation failed")
	t.Logf("Linearization: %s", result2.Linearization.Status)
	t.Logf("Watch validation: %s", result2.Watch.Status)
	t.Logf("Serializable validation: %s", result2.Serializable.Status)
}

// extractPersistedRequests extracts WAL data from logs, merges entries, and
// parses them into persisted requests for validation.
func extractPersistedRequests(t *testing.T, logs []map[string]any) []model.EtcdRequest {
	t.Helper()

	var walReports []walReportData
	for _, logEntry := range logs {
		if logEntry["msg"] != "gosim_wal_report" {
			continue
		}
		dataStr, ok := logEntry["data"].(string)
		if !ok {
			continue
		}
		var wr walReportData
		if err := json.Unmarshal([]byte(dataStr), &wr); err != nil {
			t.Logf("failed to parse WAL report: %v", err)
			continue
		}
		walReports = append(walReports, wr)
	}

	if len(walReports) == 0 {
		t.Logf("no WAL reports found in logs")
		return nil
	}

	memberEntries := make([][]raftpb.Entry, len(walReports))
	var minCommitIndex uint64 = math.MaxUint64
	for i, wr := range walReports {
		stateBytes, err := base64.StdEncoding.DecodeString(wr.HardState)
		if err != nil {
			t.Logf("failed to decode hard state for %s: %v", wr.NodeName, err)
			return nil
		}
		var state raftpb.HardState
		if err := state.Unmarshal(stateBytes); err != nil {
			t.Logf("failed to unmarshal hard state for %s: %v", wr.NodeName, err)
			return nil
		}
		minCommitIndex = min(minCommitIndex, state.Commit)

		entries := make([]raftpb.Entry, len(wr.Entries))
		for j, ed := range wr.Entries {
			entBytes, err := base64.StdEncoding.DecodeString(ed.Data)
			if err != nil {
				t.Logf("failed to decode entry %d for %s: %v", j, wr.NodeName, err)
				return nil
			}
			if err := entries[j].Unmarshal(entBytes); err != nil {
				t.Logf("failed to unmarshal entry %d for %s: %v", j, wr.NodeName, err)
				return nil
			}
		}
		memberEntries[i] = entries
		t.Logf("  WAL %s: %d entries, commit=%d", wr.NodeName, len(entries), state.Commit)
	}

	mergedEntries, err := report.MergeMembersEntries(minCommitIndex, memberEntries)
	if err != nil {
		t.Logf("failed to merge WAL entries: %v", err)
		return nil
	}

	persistedRequests := make([]model.EtcdRequest, 0, len(mergedEntries))
	for _, e := range mergedEntries {
		if e.Type != raftpb.EntryNormal {
			continue
		}
		request, err := report.ParseEntryNormal(e)
		if err != nil {
			t.Logf("failed to parse WAL entry: %v", err)
			return nil
		}
		if request != nil {
			persistedRequests = append(persistedRequests, *request)
		}
	}

	return persistedRequests
}

// writeClientReportFiles writes NDJSON files in the format expected by report.LoadClientReports.
func writeClientReportFiles(t *testing.T, reportDir string, simReport gosimReport) {
	t.Helper()
	for _, client := range simReport.Clients {
		clientDir := filepath.Join(reportDir, fmt.Sprintf("client-%d", client.ClientID))
		require.NoError(t, os.MkdirAll(clientDir, 0o700))

		if len(client.Operations) > 0 {
			f, err := os.Create(filepath.Join(clientDir, "operations.json"))
			require.NoError(t, err)
			for _, op := range client.Operations {
				data, err := json.Marshal(op)
				require.NoError(t, err)
				f.Write(data)
				f.WriteString("\n")
			}
			f.Close()
		}

		if len(client.Watch) > 0 {
			f, err := os.Create(filepath.Join(clientDir, "watch.json"))
			require.NoError(t, err)
			for _, watchOp := range client.Watch {
				data, err := json.Marshal(watchOp)
				require.NoError(t, err)
				f.Write(data)
				f.WriteString("\n")
			}
			f.Close()
		}
	}
}
