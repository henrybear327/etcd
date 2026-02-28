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
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"sync"
	"time"

	"go.uber.org/zap"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"go.etcd.io/etcd/tests/v3/robustness/identity"
	"go.etcd.io/etcd/tests/v3/robustness/model"
)

// recordingClient wraps a clientv3.Client and records all operations
// in a format compatible with the robustness validation framework.
//
// With gosimruntime.SetNanotimeStep(1), every time.Since(baseTime) call
// returns a unique, strictly increasing value. No timestamp hacks needed.
type recordingClient struct {
	id       int
	client   *clientv3.Client
	baseTime time.Time

	mu           sync.Mutex
	kvOperations *model.AppendableHistory

	watchMu         sync.Mutex
	watchOperations []model.WatchOperation
}

func newRecordingClient(endpoints []string, ids identity.Provider, baseTime time.Time) (*recordingClient, error) {
	cc, err := clientv3.New(clientv3.Config{
		Endpoints:            endpoints,
		Logger:               zap.NewNop(),
		DialKeepAliveTime:    10 * time.Second,
		DialKeepAliveTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		return nil, err
	}
	return &recordingClient{
		id:           ids.NewClientID(),
		client:       cc,
		kvOperations: model.NewAppendableHistory(ids),
		baseTime:     baseTime,
	}, nil
}

func (c *recordingClient) Close() error {
	return c.client.Close()
}

// --- Serialization types ---

type operationRecord struct {
	ClientID int                     `json:"ClientId"`
	Input    model.EtcdRequest       `json:"Input"`
	Call     int64                   `json:"Call"`
	Output   model.MaybeEtcdResponse `json:"Output"`
	Return   int64                   `json:"Return"`
}

// clientReportData holds a single client's report for JSON serialization.
type clientReportData struct {
	ClientID   int                    `json:"client_id"`
	Operations []operationRecord      `json:"operations"`
	Watch      []model.WatchOperation `json:"watch"`
}

// gosimReport is the top-level report structure.
type gosimReport struct {
	Clients []clientReportData `json:"clients"`
}

func serializeReports(clients []*recordingClient) {
	var report gosimReport
	for _, c := range clients {
		ops := c.kvOperations.History.Operations()
		records := make([]operationRecord, 0, len(ops))
		for _, op := range ops {
			records = append(records, operationRecord{
				ClientID: op.ClientId,
				Input:    op.Input.(model.EtcdRequest),
				Call:     op.Call,
				Output:   op.Output.(model.MaybeEtcdResponse),
				Return:   op.Return,
			})
		}
		report.Clients = append(report.Clients, clientReportData{
			ClientID:   c.id,
			Operations: records,
			Watch:      c.watchOperations,
		})
	}

	data, err := json.Marshal(report)
	if err != nil {
		log.Fatalf("failed to serialize report: %v", err)
	}
	slog.Info("gosim_report", "report_json", string(data))
}

func toWatchResponse(r clientv3.WatchResponse, baseTime time.Time) model.WatchResponse {
	resp := model.WatchResponse{Time: time.Since(baseTime)}
	for _, event := range r.Events {
		resp.Events = append(resp.Events, toWatchEvent(*event))
	}
	resp.IsProgressNotify = r.IsProgressNotify()
	resp.Revision = r.Header.Revision
	if err := r.Err(); err != nil {
		resp.Error = err.Error()
	}
	return resp
}

func toWatchEvent(event clientv3.Event) model.WatchEvent {
	watch := model.WatchEvent{}
	watch.Revision = event.Kv.ModRevision
	watch.Key = string(event.Kv.Key)
	watch.Value = model.ToValueOrHash(string(event.Kv.Value))

	if event.PrevKv != nil {
		watch.PrevValue = &model.ValueRevision{
			Value:       model.ToValueOrHash(string(event.PrevKv.Value)),
			ModRevision: event.PrevKv.ModRevision,
			Version:     event.PrevKv.Version,
		}
	}
	watch.IsCreate = event.IsCreate()

	switch event.Type {
	case mvccpb.Event_PUT:
		watch.Type = model.PutOperation
	case mvccpb.Event_DELETE:
		watch.Type = model.DeleteOperation
	default:
		panic(fmt.Sprintf("unexpected event type: %s", event.Type))
	}
	return watch
}
