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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"go.etcd.io/etcd/client/pkg/v3/fileutil"
	"go.etcd.io/etcd/server/v3/storage/datadir"
	"go.etcd.io/etcd/server/v3/storage/wal"
	"go.etcd.io/etcd/server/v3/storage/wal/walpb"
	"go.etcd.io/raft/v3/raftpb"
)

// walEntryData holds a single raft entry serialized as base64 protobuf.
type walEntryData struct {
	Data string `json:"data"` // base64-encoded protobuf-marshaled raftpb.Entry
}

// walReportData holds WAL data for a single node.
type walReportData struct {
	NodeName  string         `json:"node_name"`
	HardState string         `json:"hard_state"` // base64-encoded protobuf-marshaled raftpb.HardState
	Entries   []walEntryData `json:"entries"`
}

// readAndReportWAL reads WAL files from the etcd data directory and reports
// them via slog for extraction by the runner.
func readAndReportWAL(nodeName, dataDir string) {
	walDir := datadir.ToWALDir(dataDir)
	state, entries, err := readAllWALEntries(walDir)
	if err != nil {
		log.Printf("WAL read failed for %s: %v", nodeName, err)
		return
	}

	// Serialize hard state
	stateBytes, err := state.Marshal()
	if err != nil {
		log.Printf("failed to marshal hard state for %s: %v", nodeName, err)
		return
	}

	// Serialize entries
	entryDatas := make([]walEntryData, len(entries))
	for i, e := range entries {
		eBytes, err := e.Marshal()
		if err != nil {
			log.Printf("failed to marshal entry %d for %s: %v", i, nodeName, err)
			return
		}
		entryDatas[i] = walEntryData{Data: base64.StdEncoding.EncodeToString(eBytes)}
	}

	report := walReportData{
		NodeName:  nodeName,
		HardState: base64.StdEncoding.EncodeToString(stateBytes),
		Entries:   entryDatas,
	}

	data, err := json.Marshal(report)
	if err != nil {
		log.Printf("failed to serialize WAL report for %s: %v", nodeName, err)
		return
	}
	slog.Info("gosim_wal_report", "node", nodeName, "data", string(data))
	log.Printf("WAL report for %s: %d entries, commit=%d", nodeName, len(entries), state.Commit)
}

// readAllWALEntries reads all WAL entries from the given directory.
// Re-implemented from report/wal.go because the report package imports
// framework/e2e which can't run under gosim.
func readAllWALEntries(dirpath string) (state raftpb.HardState, ents []raftpb.Entry, err error) {
	names, err := fileutil.ReadDir(dirpath)
	if err != nil {
		return state, nil, err
	}
	files := make([]fileutil.FileReader, 0, len(names))
	for _, name := range names {
		if !strings.HasSuffix(name, ".wal") {
			continue
		}
		p := filepath.Join(dirpath, name)
		f, err := os.OpenFile(p, os.O_RDONLY, fileutil.PrivateFileMode)
		if err != nil {
			return state, nil, fmt.Errorf("os.OpenFile failed (%q): %w", p, err)
		}
		defer f.Close()
		files = append(files, fileutil.NewFileReader(f))
	}
	rec := &walpb.Record{}
	decoder := wal.NewDecoder(files...)
	for err = decoder.Decode(rec); err == nil; err = decoder.Decode(rec) {
		switch rec.GetType() {
		case wal.EntryType:
			e := wal.MustUnmarshalEntry(rec.Data)
			i := len(ents)
			for ; i > 0 && ents[i-1].Index >= e.Index; i-- {
			}
			ents = append(ents[:i], e)
		case wal.StateType:
			state = wal.MustUnmarshalState(rec.Data)
		case wal.MetadataType:
		case wal.CrcType:
			crc := decoder.LastCRC()
			if crc != 0 && rec.Validate(crc) != nil {
				state.Reset()
				return state, nil, wal.ErrCRCMismatch
			}
			decoder.UpdateCRC(rec.GetCrc())
		case wal.SnapshotType:
		default:
			return state, nil, fmt.Errorf("unexpected block type %d", rec.Type)
		}
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return state, nil, err
	}
	return state, ents, nil
}
