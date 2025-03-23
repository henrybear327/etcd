package client

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/tests/v3/framework/e2e"
	"go.etcd.io/etcd/tests/v3/robustness/identity"
	"go.etcd.io/etcd/tests/v3/robustness/report"
)

/*
There are 3 cases where we are trying to cover with the KVHash checker go routine:
- after each compaction, compute the hash against the compact revision
- at the end of the test, we verify all members have exactly the same hash value on the key space
- randomly compute hash on a random historic revision during the test

Notice that:
- The design need to exclude false alarm, as the compaction is executed in the applying workflow/path, but hashKV is executed in the API layer
  - TODO: how?

- The CompactRevision and HashRevision must be the same in the HashKVResponse of all members, otherwise it makes no sense to compare the Hash

TODO/FIXME:
- look for // TODO: add a revision KVHash query here
- how do you run this until the very end of the test though?
- randomly compute hash on a random historic revision during the test
*/
func ClusterKVHashChecks(ctx context.Context, t *testing.T, clus *e2e.EtcdProcessCluster, hashKVRevisionChan <-chan int64, baseTime time.Time, ids identity.Provider) []report.ClientReport {
	/*
		The caller supplies the revision that we would like to check the hash for through hashKVRevisionChan,
		and then this go routine will keep on collecting the HashKVResponse until the end of the test, and see if we find any mismatches
	*/

	mux := sync.Mutex{}
	var wg sync.WaitGroup
	reports := make([]report.ClientReport, len(clus.Procs))
	memberHashKVRevisionChans := make([]chan int64, len(clus.Procs))
	for i, member := range clus.Procs {
		c, err := NewRecordingClient(member.EndpointsGRPC(), ids, baseTime)
		require.NoError(t, err)
		memberHashKVRevisionChans[i] = make(chan int64, 100) // use a buffer in case the checks are coming in a rapid pace
		wg.Add(1)
		go func(i int, c *RecordingClient) {
			defer wg.Done()
			defer c.Close()
			checkKVHash(ctx, t, c, hashKVRevisionChan)
			mux.Lock()
			reports[i] = c.Report() // TODO: do we need to save a report? And do we need to add this call to the replay functionality?
			mux.Unlock()
		}(i, c)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			if revision, ok := <-hashKVRevisionChan; ok {
				for _, memberChan := range memberHashKVRevisionChans {
					memberChan <- revision
				}
			} else {
				break
			}
		}
	}()
	wg.Wait()
	return reports
}

// checkKVHash checks all server's KVHash value until either of the following condition is met
// - context is cancelled
// - it has observed revision provided via maxRevisionChan
// - maxRevisionChan was closed.
func checkKVHash(ctx context.Context, t *testing.T, c *RecordingClient, hashKVRevisionChan <-chan int64) {
	var maxRevision int64
	var lastRevision int64 = 1
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
resetWatch:
	for {
		watch := c.Watch(ctx, "", lastRevision+1, true, true, false)
		for {
			select {
			case <-ctx.Done():
				if maxRevision == 0 {
					t.Errorf("Client didn't collect all events, max revision not set")
				}
				if lastRevision < maxRevision {
					t.Errorf("Client didn't collect all events, revision got %d, expected: %d", lastRevision, maxRevision)
				}
				return
			case revision, ok := <-hashKVRevisionChan:
				if ok {
					maxRevision = revision
					if lastRevision >= maxRevision {
						cancel()
					}
				} else {
					// Only cancel if maxRevision was never set.
					if maxRevision == 0 {
						cancel()
					}
				}
			case resp, ok := <-watch:
				if !ok {
					t.Logf("Watch channel closed")
					continue resetWatch
				}

				if resp.Err() != nil {
					if resp.Canceled {
						if resp.CompactRevision > lastRevision {
							lastRevision = resp.CompactRevision
						}
						continue resetWatch
					}
					t.Errorf("Watch stream received error, err %v", resp.Err())
				}
				if len(resp.Events) > 0 {
					lastRevision = resp.Events[len(resp.Events)-1].Kv.ModRevision
				}
				if maxRevision != 0 && lastRevision >= maxRevision {
					cancel()
				}
			}
		}
	}
}
