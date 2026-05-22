// Copyright 2026 The etcd Authors
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

//go:build antithesis

package randutil

import (
	"math/rand"
	"sync"

	antirand "github.com/antithesishq/antithesis-sdk-go/random"

	clientv3 "go.etcd.io/etcd/client/v3"
)

var (
	randMu sync.Mutex
	rng    = rand.New(antirand.Source())
)

func init() {
	clientv3.RandFloat64 = Float64
}

func Float64() float64 {
	randMu.Lock()
	defer randMu.Unlock()
	return rng.Float64()
}

func Intn(n int) int {
	randMu.Lock()
	defer randMu.Unlock()
	return rng.Intn(n)
}

func Int63n(n int64) int64 {
	randMu.Lock()
	defer randMu.Unlock()
	return rng.Int63n(n)
}
