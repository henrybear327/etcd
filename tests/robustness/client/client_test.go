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

package client

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/tests/v3/framework/e2e"
	"go.etcd.io/etcd/tests/v3/robustness/identity"
	"go.etcd.io/etcd/tests/v3/robustness/model"
)

func TestToWatchEvent(t *testing.T) {
	testCases := []struct {
		name     string
		event    clientv3.Event
		expected model.WatchEvent
	}{
		{
			name: "Put event",
			event: clientv3.Event{
				Type: mvccpb.PUT,
				Kv: &mvccpb.KeyValue{
					Key:         []byte("key"),
					Value:       []byte("value"),
					ModRevision: 2,
					CreateRevision: 2, // Add CreateRevision and set it to ModRevision
					Version:     1,
				},
			},
			expected: model.WatchEvent{
				PersistedEvent: model.PersistedEvent{
					Event: model.Event{
						Type:  model.PutOperation,
						Key:   "key",
						Value: model.ToValueOrHash("value"),
					},
					Revision: 2,
					IsCreate: true,
				},
			},
		},
		{
			name: "Delete event",
			event: clientv3.Event{
				Type: mvccpb.DELETE,
				Kv: &mvccpb.KeyValue{
					Key:         []byte("key"),
					ModRevision: 3,
				},
			},
			expected: model.WatchEvent{
				PersistedEvent: model.PersistedEvent{
					Event: model.Event{
						Type:  model.DeleteOperation,
						Key:   "key",
						Value: model.ToValueOrHash(""),
					},
					Revision: 3,
				},
			},
		},
		{
			name: "Put event with previous kv",
			event: clientv3.Event{
				Type: mvccpb.PUT,
				Kv: &mvccpb.KeyValue{
					Key:         []byte("key"),
					Value:       []byte("new-value"),
					ModRevision: 4,
				},
				PrevKv: &mvccpb.KeyValue{
					Key:         []byte("key"),
					Value:       []byte("old-value"),
					ModRevision: 3,
					Version:     1,
				},
			},
			expected: model.WatchEvent{
				PersistedEvent: model.PersistedEvent{
					Event: model.Event{
						Type:  model.PutOperation,
						Key:   "key",
						Value: model.ToValueOrHash("new-value"),
					},
					Revision: 4,
				},
				PrevValue: &model.ValueRevision{
					Value:       model.ToValueOrHash("old-value"),
					ModRevision: 3,
					Version:     1,
				},
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			actual := toWatchEvent(tc.event)
			assert.Equal(t, tc.expected, actual)
		})
	}
}

func TestToWatchResponse(t *testing.T) {
	baseTime := time.Now()
	time.Sleep(10 * time.Millisecond) // ensure time progresses
	testCases := []struct {
		name     string
		response clientv3.WatchResponse
		validate func(t *testing.T, resp model.WatchResponse)
	}{
		{
			name: "Single event",
			response: clientv3.WatchResponse{
				Header: etcdserverpb.ResponseHeader{Revision: 1},
				Events: []*clientv3.Event{
					{
						Type: mvccpb.PUT,
						Kv: &mvccpb.KeyValue{
							Key:         []byte("key"),
							Value:       []byte("value"),
							ModRevision: 2,
						},
					},
				},
			},
			validate: func(t *testing.T, resp model.WatchResponse) {
				assert.Greater(t, resp.Time, time.Duration(0))
				assert.False(t, resp.IsProgressNotify)
				assert.Equal(t, int64(1), resp.Revision)
				assert.Empty(t, resp.Error)
				assert.Len(t, resp.Events, 1)
				assert.Equal(t, model.PutOperation, resp.Events[0].PersistedEvent.Event.Type)
				assert.Equal(t, "key", resp.Events[0].PersistedEvent.Event.Key)
			},
		},
		{
			name: "Progress notify",
			response: clientv3.WatchResponse{
				Header: etcdserverpb.ResponseHeader{Revision: 2},
			},
			validate: func(t *testing.T, resp model.WatchResponse) {
				assert.Greater(t, resp.Time, time.Duration(0))
				assert.True(t, resp.IsProgressNotify)
				assert.Equal(t, int64(2), resp.Revision)
				assert.Empty(t, resp.Error)
				assert.Empty(t, resp.Events)
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			actual := ToWatchResponse(tc.response, baseTime)
			tc.validate(t, actual)
		})
	}
}

func TestClientSet(t *testing.T) {
	e2e.BeforeTest(t)
	clus, err := e2e.NewEtcdProcessCluster(context.Background(), t, e2e.WithClusterSize(1))
	require.NoError(t, err)
	defer clus.Close()

	idProvider := identity.NewIDProvider()
	baseTime := time.Now()
	cs := NewSet(idProvider, baseTime)

	assert.Equal(t, idProvider, cs.IdentityProvider())
	assert.Equal(t, baseTime, cs.BaseTime())

	endpoints := []string{clus.Procs[0].Config().ClientURL}
	// Create a client
	c, err := cs.NewClient(endpoints)
	require.NoError(t, err)
	assert.NotNil(t, c)

	// Closing the client set
	cs.Close()

	// Cannot create new client on closed set
	_, err = cs.NewClient(endpoints)
	assert.Equal(t, errors.New("the clientset is already closed"), err)

	// Reports are available after close
	reports := cs.Reports()
	assert.Len(t, reports, 1)
}

func TestRecordingClient(t *testing.T) {
	e2e.BeforeTest(t)
	clus, err := e2e.NewEtcdProcessCluster(context.Background(), t, e2e.WithClusterSize(1))
	require.NoError(t, err)
	defer clus.Close()

	idProvider := identity.NewIDProvider()
	baseTime := time.Now()
	endpoints := []string{clus.Procs[0].Config().ClientURL}
	c, err := NewRecordingClient(endpoints, idProvider, baseTime)
	require.NoError(t, err)
	defer c.Close()

	ctx := context.Background()

	t.Run("Put", func(t *testing.T) {
		_, err = c.Put(ctx, "key", "value")
		require.NoError(t, err)

		report := c.Report()
		require.NotEmpty(t, report.KeyValue)
		lastOp := report.KeyValue[len(report.KeyValue)-1]
		request := lastOp.Input.(model.EtcdRequest)
		assert.Equal(t, model.Txn, request.Type)
		require.Len(t, request.Txn.OperationsOnSuccess, 1)
		putOp := request.Txn.OperationsOnSuccess[0]
		assert.Equal(t, model.PutOperation, putOp.Type)
		assert.Equal(t, "key", putOp.Put.Key)
		assert.Equal(t, model.ToValueOrHash("value"), putOp.Put.Value)
	})

	t.Run("Get", func(t *testing.T) {
		resp, err := c.Get(ctx, "key")
		require.NoError(t, err)
		assert.Equal(t, "value", string(resp.Kvs[0].Value))

		report := c.Report()
		require.NotEmpty(t, report.KeyValue)
		lastOp := report.KeyValue[len(report.KeyValue)-1]
		request := lastOp.Input.(model.EtcdRequest)
		assert.Equal(t, model.Range, request.Type)
		assert.Equal(t, "key", request.Range.Start)
	})

	t.Run("Delete", func(t *testing.T) {
		_, err := c.Delete(ctx, "key")
		require.NoError(t, err)

		report := c.Report()
		require.NotEmpty(t, report.KeyValue)
		lastOp := report.KeyValue[len(report.KeyValue)-1]
		request := lastOp.Input.(model.EtcdRequest)
		assert.Equal(t, model.Txn, request.Type)
		require.Len(t, request.Txn.OperationsOnSuccess, 1)
		deleteOp := request.Txn.OperationsOnSuccess[0]
		assert.Equal(t, model.DeleteOperation, deleteOp.Type)
		assert.Equal(t, "key", deleteOp.Delete.Key)
	})

	t.Run("Txn", func(t *testing.T) {
		_, err := c.Put(ctx, "txn-key", "value")
		require.NoError(t, err)

		_, err = c.Txn(ctx).
			If(clientv3.Compare(clientv3.Value("txn-key"), "=", "value")).
			Then(clientv3.OpPut("txn-key", "new-value")).
			Else(clientv3.OpPut("txn-key", "unexpected-value")).
			Commit()
		require.NoError(t, err)

		resp, err := c.Get(ctx, "txn-key")
		require.NoError(t, err)
		assert.Equal(t, "new-value", string(resp.Kvs[0].Value))

		report := c.Report()
		require.NotEmpty(t, report.KeyValue)
		lastOp := report.KeyValue[len(report.KeyValue)-2] // -2 because of the get
		request := lastOp.Input.(model.EtcdRequest)
		assert.Equal(t, model.Txn, request.Type)
		assert.Len(t, request.Txn.Conditions, 1)
		assert.Len(t, request.Txn.OperationsOnSuccess, 1)
		assert.Len(t, request.Txn.OperationsOnFailure, 1)
	})
}

func TestRecordingClientWatch(t *testing.T) {
	e2e.BeforeTest(t)
	clus, err := e2e.NewEtcdProcessCluster(context.Background(), t, e2e.WithClusterSize(1))
	require.NoError(t, err)
	defer clus.Close()

	idProvider := identity.NewIDProvider()
	baseTime := time.Now()
	endpoints := []string{clus.Procs[0].Config().ClientURL}
	c, err := NewRecordingClient(endpoints, idProvider, baseTime)
	require.NoError(t, err)
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	numWatches := 5
	var wg sync.WaitGroup
	wg.Add(numWatches)

	watchChs := make([]clientv3.WatchChan, numWatches)
	for i := 0; i < numWatches; i++ {
		go func(i int) {
			defer wg.Done()
			watchChs[i] = c.Watch(ctx, "key", 0, false, false, false)
		}(i)
	}

	wg.Wait()

	for i := 0; i < numWatches; i++ {
		require.NotNil(t, watchChs[i])
	}

	// Now, let's put a key and see if all watches receive it.
	_, err = c.Put(ctx, "key", "value")
	require.NoError(t, err)

	for i := 0; i < numWatches; i++ {
		select {
		case resp := <-watchChs[i]:
			require.Len(t, resp.Events, 1)
			assert.Equal(t, "key", string(resp.Events[0].Kv.Key))
			assert.Equal(t, "value", string(resp.Events[0].Kv.Value))
		case <-ctx.Done():
			t.Fatal("watch timed out")
		}
	}

	report := c.Report()
	require.Len(t, report.Watch, numWatches)
	for _, op := range report.Watch {
		assert.Equal(t, "key", op.Request.Key)
		assert.Len(t, op.Responses, 1)
		assert.Len(t, op.Responses[0].Events, 1)
		assert.Equal(t, "key", op.Responses[0].Events[0].PersistedEvent.Event.Key)
		assert.Equal(t, model.ToValueOrHash("value"), op.Responses[0].Events[0].PersistedEvent.Event.Value)
	}
}

func TestIsCreate(t *testing.T) {
	event := clientv3.Event{
		Type: mvccpb.PUT,
		Kv: &mvccpb.KeyValue{
			Key:         []byte("key"),
			Value:       []byte("value"),
			ModRevision: 2,
			CreateRevision: 2, // Add CreateRevision and set it to ModRevision
			Version:     1,
		},
		PrevKv: nil, // Explicitly nil
	}
	watchEvent := toWatchEvent(event)
	assert.True(t, watchEvent.PersistedEvent.IsCreate)
}