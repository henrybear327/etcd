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

	"go.uber.org/zap"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/tests/v3/framework/e2e"
	"go.etcd.io/etcd/tests/v3/robustness/identity"
)

func getHashKV(ctx context.Context, t *testing.T, endpoints []string, rev int64) (int64, *clientv3.HashKVResponse) {
	c, err := clientv3.New(clientv3.Config{
		Endpoints:            endpoints,
		Logger:               zap.NewNop(),
		DialKeepAliveTime:    10 * time.Second,
		DialKeepAliveTimeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Error(err)
		return 0, nil
	}
	defer c.Close()

	hashKV, err := c.HashKV(ctx, c.Endpoints()[0], rev)
	if err != nil {
		t.Error(err)
		return 0, nil
	}

	return hashKV.Header.Revision, hashKV
}

func CheckHashKV(ctx context.Context, t *testing.T, clus *e2e.EtcdProcessCluster, rev int64, baseTime time.Time, ids identity.Provider) {
	hashKVs := make([]*clientv3.HashKVResponse, 0)
	if rev == 0 {
		maxRevision, hashKV := getHashKV(ctx, t, clus.EndpointsGRPC(), rev)
		rev = maxRevision
		hashKVs = append(hashKVs, hashKV)
	}

	for _, member := range clus.Procs {
		maxRevision, hashKV := getHashKV(ctx, t, member.EndpointsGRPC(), rev)
		if maxRevision != rev {
			t.Errorf("Max revision between nodes should be the same. Want %v, get %v", rev, maxRevision)
		}
		hashKVs = append(hashKVs, hashKV)
	}

	for i := 1; i < len(clus.Procs); i++ {
		if hashKVs[i-1].HashRevision != hashKVs[i].HashRevision {
			t.Error("HashRevision mismatch")
		}
		if hashKVs[i-1].CompactRevision != hashKVs[i].CompactRevision {
			t.Error("CompactRevision mismatch")
		}
		if hashKVs[i-1].Hash != hashKVs[i].Hash {
			t.Error("Hash mismatch")
		}
	}
}
