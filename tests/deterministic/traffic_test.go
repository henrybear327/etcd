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
	"fmt"
	"log"
	"math/rand/v2"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	"golang.org/x/time/rate"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/tests/v3/robustness/identity"
	"go.etcd.io/etcd/tests/v3/robustness/model"
)

// --- Request types and traffic profile ---

type etcdRequestType string

const (
	Get           etcdRequestType = "get"
	StaleGet      etcdRequestType = "staleGet"
	List          etcdRequestType = "list"
	StaleList     etcdRequestType = "staleList"
	Put           etcdRequestType = "put"
	Delete        etcdRequestType = "delete"
	MultiOpTxn    etcdRequestType = "multiOpTxn"
	PutWithLease  etcdRequestType = "putWithLease"
	LeaseRevoke   etcdRequestType = "leaseRevoke"
	CompareAndSet etcdRequestType = "compareAndSet"
)

const (
	defaultLeaseTTL   int64 = 7200
	multiOpTxnOpCount       = 4
)

type choiceWeight struct {
	choice etcdRequestType
	weight int
}

// etcdPutDeleteLease matches the EtcdPutDeleteLease traffic profile from
// robustness/traffic/etcd.go. Sum of weights = 100.
var etcdPutDeleteLease = []choiceWeight{
	{choice: Get, weight: 15},
	{choice: List, weight: 15},
	{choice: StaleGet, weight: 10},
	{choice: StaleList, weight: 10},
	{choice: Delete, weight: 5},
	{choice: MultiOpTxn, weight: 10},
	{choice: PutWithLease, weight: 5},
	{choice: LeaseRevoke, weight: 5},
	{choice: CompareAndSet, weight: 5},
	{choice: Put, weight: 20},
}

var etcdPut = []choiceWeight{
	{choice: Get, weight: 15},
	{choice: List, weight: 15},
	{choice: StaleGet, weight: 10},
	{choice: StaleList, weight: 10},
	{choice: MultiOpTxn, weight: 10},
	{choice: Put, weight: 40},
}

var etcdDelete = []choiceWeight{
	{choice: Put, weight: 50},
	{choice: Delete, weight: 50},
}

var trafficProfiles = map[string][]choiceWeight{
	"EtcdPutDeleteLease": etcdPutDeleteLease,
	"EtcdPut":            etcdPut,
	"EtcdDelete":         etcdDelete,
}

func pickRandom(choices []choiceWeight) etcdRequestType {
	sum := 0
	for _, c := range choices {
		sum += c.weight
	}
	roll := rand.IntN(sum)
	for _, c := range choices {
		if roll < c.weight {
			return c.choice
		}
		roll -= c.weight
	}
	panic("unexpected")
}

func filterOutNonUniqueWrites(choices []choiceWeight) []choiceWeight {
	var filtered []choiceWeight
	for _, c := range choices {
		if c.choice != Delete && c.choice != LeaseRevoke {
			filtered = append(filtered, c)
		}
	}
	return filtered
}

// --- Key store ---

type keyStore struct {
	mu             sync.Mutex
	counter        int
	keys           []string
	keyPrefix      string
	latestRevision int64
}

func newKeyStore(size int, prefix string) *keyStore {
	k := &keyStore{
		keys:      make([]string, size),
		counter:   0,
		keyPrefix: prefix,
	}
	for ; k.counter < len(k.keys); k.counter++ {
		k.keys[k.counter] = fmt.Sprintf("%s%d", k.keyPrefix, k.counter)
	}
	return k
}

func (k *keyStore) GetKey() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.keys[rand.IntN(len(k.keys))]
}

func (k *keyStore) GetKeyForDelete() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	idx := rand.IntN(len(k.keys))
	key := k.keys[idx]
	k.replaceKey(idx)
	return key
}

func (k *keyStore) GetKeysForMultiTxnOps(ops []model.OperationType) []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	numOps := len(ops)
	if numOps > len(k.keys) {
		panic("GetKeysForMultiTxnOps: number of operations exceeds key pool size")
	}
	keys := make([]string, numOps)
	perm := rand.Perm(len(k.keys))
	for i, op := range ops {
		keys[i] = k.keys[perm[i]]
		if op == model.DeleteOperation {
			k.replaceKey(perm[i])
		}
	}
	return keys
}

func (k *keyStore) GetPrefix() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.keyPrefix
}

func (k *keyStore) SyncKeys(resp *clientv3.GetResponse) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if resp.Header.GetRevision() < k.latestRevision {
		return
	}
	k.latestRevision = resp.Header.GetRevision()
	listKeys := make([]string, len(resp.Kvs))
	for i, kv := range resp.Kvs {
		listKeys[i] = string(kv.Key)
	}
	for _, key := range k.keys {
		if !slices.Contains(listKeys, key) {
			listKeys = append(listKeys, key)
		}
	}
	sort.Slice(listKeys, func(i, j int) bool {
		return k.getKeyNum(listKeys[i]) < k.getKeyNum(listKeys[j])
	})
	k.keys = listKeys[:len(k.keys)]
	lastKeyNum := k.getKeyNum(k.keys[len(k.keys)-1])
	k.counter = lastKeyNum + 1
}

func (k *keyStore) getKeyNum(key string) int {
	if len(key) < len(k.keyPrefix) {
		return 0
	}
	numStr := key[len(k.keyPrefix):]
	num, _ := strconv.Atoi(numStr)
	return num
}

func (k *keyStore) replaceKey(index int) {
	if rand.IntN(100) < 90 {
		k.keys[index] = fmt.Sprintf("%s%d", k.keyPrefix, k.counter)
		k.counter++
	}
}

// --- Concurrency limiter ---

type concurrencyLimiter struct {
	ch chan struct{}
}

func newConcurrencyLimiter(size int) *concurrencyLimiter {
	return &concurrencyLimiter{
		ch: make(chan struct{}, size),
	}
}

func (c *concurrencyLimiter) Take() bool {
	select {
	case c.ch <- struct{}{}:
		return true
	default:
		return false
	}
}

func (c *concurrencyLimiter) Return() {
	select {
	case <-c.ch:
	default:
		panic("Call to Return() without a successful Take")
	}
}

// --- Traffic runner ---

type trafficClient struct {
	client       *recordingClient
	keyStore     *keyStore
	limiter      *rate.Limiter
	idProvider   identity.Provider
	leaseStorage identity.LeaseIDStorage
	baseTime     time.Time
}

// runFullKVTraffic runs the full antithesis-parity traffic pattern using a finish channel.
func runFullKVTraffic(tc *trafficClient, finish <-chan struct{}, updateMaxRev func(int64), nonUniqueLimiter *concurrencyLimiter) {
	lastOperationSucceeded := true
	var lastRev int64
	for {
		select {
		case <-finish:
			return
		default:
		}

		var requestType etcdRequestType
		shouldReturn := false

		if lastOperationSucceeded {
			choices := etcdPutDeleteLease
			shouldReturn = nonUniqueLimiter.Take()
			if !shouldReturn {
				choices = filterOutNonUniqueWrites(choices)
			}
			requestType = pickRandom(choices)
		} else {
			requestType = List
		}

		rev, err := executeRequest(tc, requestType, lastRev)
		if shouldReturn {
			nonUniqueLimiter.Return()
		}
		lastOperationSucceeded = err == nil
		if err != nil {
			continue
		}
		if rev != 0 {
			lastRev = rev
			updateMaxRev(rev)
		}
		if err := tc.limiter.Wait(context.Background()); err != nil {
			return
		}
	}
}

// runTrafficOps runs count weighted-random operations using the given profile.
// Returns the maximum revision observed, for use in watch draining.
func runTrafficOps(tc *trafficClient, profile []choiceWeight, count int, nonUniqueLimiter *concurrencyLimiter) int64 {
	lastOperationSucceeded := true
	var lastRev int64
	var maxRevision int64

	for i := 0; i < count; i++ {
		var requestType etcdRequestType
		shouldReturn := false

		if lastOperationSucceeded {
			choices := profile
			shouldReturn = nonUniqueLimiter.Take()
			if !shouldReturn {
				choices = filterOutNonUniqueWrites(choices)
			}
			requestType = pickRandom(choices)
		} else {
			requestType = List
		}

		rev, err := executeRequest(tc, requestType, lastRev)
		if shouldReturn {
			nonUniqueLimiter.Return()
		}
		lastOperationSucceeded = err == nil
		if err != nil {
			continue
		}
		if rev != 0 {
			lastRev = rev
			if rev > maxRevision {
				maxRevision = rev
			}
		}
		if err := tc.limiter.Wait(context.Background()); err != nil {
			return maxRevision
		}
	}
	return maxRevision
}

// executeRequest executes a single traffic request and records the operation.
// With nanotimeStep=1, time.Since(baseTime) returns unique values naturally.
func executeRequest(tc *trafficClient, request etcdRequestType, lastRev int64) (rev int64, err error) {
	opCtx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()
	c := tc.client
	baseTime := tc.baseTime

	switch request {
	case StaleGet:
		key := tc.keyStore.GetKey()
		c.mu.Lock()
		callTime := time.Since(baseTime)
		var resp *clientv3.GetResponse
		resp, err = c.client.Get(opCtx, key, clientv3.WithRev(lastRev))
		returnTime := time.Since(baseTime)
		c.kvOperations.AppendRange(key, "", lastRev, 0, callTime, returnTime, resp, err)
		c.mu.Unlock()
		if err == nil && resp != nil {
			rev = resp.Header.Revision
		}

	case Get:
		key := tc.keyStore.GetKey()
		c.mu.Lock()
		callTime := time.Since(baseTime)
		var resp *clientv3.GetResponse
		resp, err = c.client.Get(opCtx, key)
		returnTime := time.Since(baseTime)
		c.kvOperations.AppendRange(key, "", 0, 0, callTime, returnTime, resp, err)
		c.mu.Unlock()
		if err == nil && resp != nil {
			rev = resp.Header.Revision
		}

	case List:
		prefix := tc.keyStore.GetPrefix()
		end := clientv3.GetPrefixRangeEnd(prefix)
		c.mu.Lock()
		callTime := time.Since(baseTime)
		var resp *clientv3.GetResponse
		resp, err = c.client.Get(opCtx, prefix, clientv3.WithRange(end))
		returnTime := time.Since(baseTime)
		c.kvOperations.AppendRange(prefix, end, 0, 0, callTime, returnTime, resp, err)
		c.mu.Unlock()
		if resp != nil {
			tc.keyStore.SyncKeys(resp)
			rev = resp.Header.Revision
		}

	case StaleList:
		prefix := tc.keyStore.GetPrefix()
		end := clientv3.GetPrefixRangeEnd(prefix)
		c.mu.Lock()
		callTime := time.Since(baseTime)
		var resp *clientv3.GetResponse
		resp, err = c.client.Get(opCtx, prefix, clientv3.WithRange(end), clientv3.WithRev(lastRev))
		returnTime := time.Since(baseTime)
		c.kvOperations.AppendRange(prefix, end, lastRev, 0, callTime, returnTime, resp, err)
		c.mu.Unlock()
		if resp != nil {
			rev = resp.Header.Revision
		}

	case Put:
		key := tc.keyStore.GetKey()
		value := fmt.Sprintf("%d", tc.idProvider.NewRequestID())
		c.mu.Lock()
		callTime := time.Since(baseTime)
		var resp *clientv3.PutResponse
		resp, err = c.client.Put(opCtx, key, value)
		returnTime := time.Since(baseTime)
		c.kvOperations.AppendPut(key, value, callTime, returnTime, resp, err)
		c.mu.Unlock()
		if resp != nil {
			rev = resp.Header.Revision
		}

	case Delete:
		key := tc.keyStore.GetKeyForDelete()
		c.mu.Lock()
		callTime := time.Since(baseTime)
		var resp *clientv3.DeleteResponse
		resp, err = c.client.Delete(opCtx, key)
		returnTime := time.Since(baseTime)
		c.kvOperations.AppendDelete(key, callTime, returnTime, resp, err)
		c.mu.Unlock()
		if resp != nil {
			rev = resp.Header.Revision
		}

	case MultiOpTxn:
		ops := pickMultiTxnOps(tc)
		c.mu.Lock()
		callTime := time.Since(baseTime)
		var resp *clientv3.TxnResponse
		resp, err = c.client.Txn(opCtx).Then(ops...).Commit()
		returnTime := time.Since(baseTime)
		c.kvOperations.AppendTxn(nil, ops, nil, callTime, returnTime, resp, err)
		c.mu.Unlock()
		if resp != nil {
			rev = resp.Header.Revision
		}

	case CompareAndSet:
		key := tc.keyStore.GetKey()
		c.mu.Lock()
		callTime := time.Since(baseTime)
		var getResp *clientv3.GetResponse
		getResp, err = c.client.Get(opCtx, key)
		returnTime := time.Since(baseTime)
		c.kvOperations.AppendRange(key, "", 0, 0, callTime, returnTime, getResp, err)
		c.mu.Unlock()
		if err != nil {
			break
		}
		rev = getResp.Header.Revision
		var expectedRevision int64
		if len(getResp.Kvs) == 1 {
			expectedRevision = getResp.Kvs[0].ModRevision
		}
		if waitErr := tc.limiter.Wait(context.Background()); waitErr != nil {
			return rev, waitErr
		}
		txnCtx, txnCancel := context.WithTimeout(context.Background(), requestTimeout)
		value := fmt.Sprintf("%d", tc.idProvider.NewRequestID())
		cmp := clientv3.Compare(clientv3.ModRevision(key), "=", expectedRevision)
		put := clientv3.OpPut(key, value)
		c.mu.Lock()
		callTime = time.Since(baseTime)
		var txnResp *clientv3.TxnResponse
		txnResp, err = c.client.Txn(txnCtx).If(cmp).Then(put).Commit()
		returnTime = time.Since(baseTime)
		c.kvOperations.AppendTxn(
			[]clientv3.Cmp{cmp},
			[]clientv3.Op{put},
			nil,
			callTime, returnTime, txnResp, err,
		)
		c.mu.Unlock()
		txnCancel()
		if txnResp != nil {
			rev = txnResp.Header.Revision
		}

	case PutWithLease:
		leaseID := tc.leaseStorage.LeaseID(c.id)
		if leaseID == 0 {
			c.mu.Lock()
			callTime := time.Since(baseTime)
			var grantResp *clientv3.LeaseGrantResponse
			grantResp, err = c.client.Grant(opCtx, defaultLeaseTTL)
			returnTime := time.Since(baseTime)
			c.kvOperations.AppendLeaseGrant(callTime, returnTime, grantResp, err)
			c.mu.Unlock()
			if grantResp != nil {
				leaseID = int64(grantResp.ID)
				rev = grantResp.ResponseHeader.Revision
			}
			if err == nil {
				tc.leaseStorage.AddLeaseID(c.id, leaseID)
				if waitErr := tc.limiter.Wait(context.Background()); waitErr != nil {
					return rev, waitErr
				}
			}
		}
		if leaseID != 0 {
			key := tc.keyStore.GetKey()
			value := fmt.Sprintf("%d", tc.idProvider.NewRequestID())
			putCtx, putCancel := context.WithTimeout(context.Background(), requestTimeout)
			c.mu.Lock()
			callTime := time.Since(baseTime)
			var putResp *clientv3.PutResponse
			putResp, err = c.client.Put(putCtx, key, value, clientv3.WithLease(clientv3.LeaseID(leaseID)))
			returnTime := time.Since(baseTime)
			c.kvOperations.AppendPutWithLease(key, value, leaseID, callTime, returnTime, putResp, err)
			c.mu.Unlock()
			putCancel()
			if putResp != nil {
				rev = putResp.Header.Revision
			}
		}

	case LeaseRevoke:
		leaseID := tc.leaseStorage.LeaseID(c.id)
		if leaseID != 0 {
			c.mu.Lock()
			callTime := time.Since(baseTime)
			var revokeResp *clientv3.LeaseRevokeResponse
			revokeResp, err = c.client.Revoke(opCtx, clientv3.LeaseID(leaseID))
			returnTime := time.Since(baseTime)
			c.kvOperations.AppendLeaseRevoke(leaseID, callTime, returnTime, revokeResp, err)
			c.mu.Unlock()
			if err == nil {
				tc.leaseStorage.RemoveLeaseID(c.id)
			}
			if revokeResp != nil {
				rev = revokeResp.Header.Revision
			}
		}

	default:
		log.Printf("unknown request type: %s", request)
	}

	return rev, err
}

func pickMultiTxnOps(tc *trafficClient) []clientv3.Op {
	opTypes := make([]model.OperationType, multiOpTxnOpCount)
	atLeastOnePut := false
	for i := range multiOpTxnOpCount {
		roll := rand.IntN(100)
		if roll < 10 {
			opTypes[i] = model.DeleteOperation
		} else if roll < 50 {
			opTypes[i] = model.RangeOperation
		} else {
			opTypes[i] = model.PutOperation
		}
		if opTypes[i] == model.PutOperation {
			atLeastOnePut = true
		}
	}
	if !atLeastOnePut {
		opTypes[0] = model.PutOperation
	}

	keys := tc.keyStore.GetKeysForMultiTxnOps(opTypes)
	var ops []clientv3.Op
	for i, opType := range opTypes {
		key := keys[i]
		switch opType {
		case model.RangeOperation:
			ops = append(ops, clientv3.OpGet(key))
		case model.PutOperation:
			value := fmt.Sprintf("%d", tc.idProvider.NewRequestID())
			ops = append(ops, clientv3.OpPut(key, value))
		case model.DeleteOperation:
			ops = append(ops, clientv3.OpDelete(key))
		default:
			panic("unsupported operation type")
		}
	}
	return ops
}
