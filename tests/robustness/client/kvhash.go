// Copyright 2025 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package client

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/tests/v3/framework/e2e"
	"go.etcd.io/etcd/tests/v3/robustness/identity"
)

func CheckHashKV(ctx context.Context, t *testing.T, clus *e2e.EtcdProcessCluster, rev int64, baseTime time.Time, ids identity.Provider) {
	HashKVs := make([]*clientv3.HashKVResponse, len(clus.Procs))
	maxRevision := int64(0)
	for i, member := range clus.Procs {
		c, err := clientv3.New(clientv3.Config{
			Endpoints:            member.EndpointsGRPC(),
			Logger:               zap.NewNop(),
			DialKeepAliveTime:    10 * time.Second,
			DialKeepAliveTimeout: 100 * time.Millisecond,
		})
		require.NoError(t, err)
		defer c.Close()

		var resp []*clientv3.HashKVResponse
		for _, ep := range c.Endpoints() {
			t.Logf("[CheckHashKV] calling endpoint %v with revision %v", c.Endpoints(), rev)
			hashKV, err := c.HashKV(ctx, ep, rev)
			require.NoError(t, err)
			resp = append(resp, hashKV)
		}
		require.Lenf(t, resp, 1, "We should only have 1 HashKVResponse per request")
		HashKVs[i] = resp[0]
		t.Logf("[CheckHashKV] member %v, resp[0] = Hash %v HashRevision %v CompactRevision %v Header.Revision %v", i, resp[0].Hash, resp[0].HashRevision, resp[0].CompactRevision, resp[0].Header.Revision)

		if maxRevision < resp[0].Header.Revision {
			maxRevision = resp[0].Header.Revision
		}
	}

	rev = maxRevision
	for i, member := range clus.Procs {
		c, err := clientv3.New(clientv3.Config{
			Endpoints:            member.EndpointsGRPC(),
			Logger:               zap.NewNop(),
			DialKeepAliveTime:    10 * time.Second,
			DialKeepAliveTimeout: 100 * time.Millisecond,
		})
		require.NoError(t, err)
		defer c.Close()

		var resp []*clientv3.HashKVResponse
		for _, ep := range c.Endpoints() {
			t.Logf("[CheckHashKV] calling endpoint %v with revision %v", c.Endpoints(), rev)
			hashKV, err := c.HashKV(ctx, ep, rev)
			require.NoError(t, err)
			resp = append(resp, hashKV)
		}
		require.Lenf(t, resp, 1, "We should only have 1 HashKVResponse per request")
		HashKVs[i] = resp[0]
		t.Logf("[CheckHashKV] member %v, resp[0] = Hash %v HashRevision %v CompactRevision %v Header.Revision %v", i, resp[0].Hash, resp[0].HashRevision, resp[0].CompactRevision, resp[0].Header.Revision)
	}

	for i := 1; i < len(clus.Procs); i++ {
		require.Equalf(t, HashKVs[i-1].HashRevision, HashKVs[i].HashRevision, "HashRevision mismatch")
		require.Equalf(t, HashKVs[i-1].CompactRevision, HashKVs[i].CompactRevision, "CompactRevision mismatch")
		require.Equalf(t, HashKVs[i-1].Hash, HashKVs[1].Hash, "Hash mismatch")
	}
}
