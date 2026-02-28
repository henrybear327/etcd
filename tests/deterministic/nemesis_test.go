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
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// verifyNodeServing issues a direct Get to a specific node endpoint to verify
// it has restarted and is serving requests. A quorum-based system will silently
// mask permanent node failures, so we must check the individual node.
func verifyNodeServing(t *testing.T, endpoint, nodeName string) {
	t.Helper()
	verifyClient, err := clientv3.New(clientv3.Config{
		Endpoints: []string{endpoint},
	})
	if err != nil {
		t.Fatalf("failed to create verify client for %s: %v", nodeName, err)
	}
	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), 5*time.Second)
	_, err = verifyClient.Get(verifyCtx, "health-check")
	verifyCancel()
	verifyClient.Close()
	if err != nil {
		t.Fatalf("node %s failed to restart and serve requests: %v", nodeName, err)
	}
}
