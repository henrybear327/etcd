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

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/tests/v3/framework/e2e"
	"go.etcd.io/etcd/tests/v3/robustness/identity"
	"go.etcd.io/etcd/tests/v3/robustness/report"
)

func CheckHashKV(ctx context.Context, t *testing.T, clus *e2e.EtcdProcessCluster, rev int64, baseTime time.Time, ids identity.Provider) []report.ClientReport {
	reports := make([]report.ClientReport, len(clus.Procs))
	HashKVs := make([]*clientv3.HashKVResponse, len(clus.Procs))
	for i, member := range clus.Procs {
		c, err := NewRecordingClient(member.EndpointsGRPC(), ids, baseTime)
		require.NoError(t, err)
		defer c.Close()

		hash, err := c.HashKV(ctx, rev)
		require.NoError(t, err)
		require.Lenf(t, hash, 1, "We should only have 1 HashKVResponse per request")
		HashKVs[i] = hash[0]

		reports[i] = c.Report()
	}

	for i := 1; i < len(clus.Procs); i++ {
		require.Equalf(t, HashKVs[i-1].HashRevision, HashKVs[i].HashRevision, "HashRevision mismatch")
		require.Equalf(t, HashKVs[i-1].CompactRevision, HashKVs[i].CompactRevision, "CompactRevision mismatch")
		require.Equalf(t, HashKVs[i-1].Hash, HashKVs[1].Hash, "Hash mismatch")
	}

	return reports
}
