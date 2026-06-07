// Copyright 2023 The etcd Authors
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

package validate

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync/atomic"
	"time"

	"github.com/anishathalye/porcupine"
	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"go.etcd.io/etcd/tests/v3/robustness/model"
)

var (
	errRespNotMatched         = errors.New("response didn't match expected")
	errFutureRevRespRequested = errors.New("request about a future rev with response")
)

type linearizationParams struct {
	keys            []string
	operations      []porcupine.Operation
	timeout         time.Duration
	limitBytes      uint64
	monitorInterval time.Duration
}

func validateLinearizableOperationsAndVisualize(lg *zap.Logger, params linearizationParams) LinearizationResult {
	monitorInterval := params.monitorInterval
	if params.limitBytes > 0 && monitorInterval == 0 {
		monitorInterval = DefaultMemMonitorInterval
	}
	lg.Info("Validating linearizable operations", zap.Duration("timeout", params.timeout), zap.Uint64("memory-limit-bytes", params.limitBytes), zap.Duration("memory-monitor-interval", monitorInterval))
	start := time.Now()

	m := model.NonDeterministicModel(params.keys)

	// We run a background goroutine to poll memory statistics instead of
	// calling runtime.ReadMemStats on every StepContext execution. ReadMemStats is relatively
	// expensive and would slow down validation drastically if called on every DFS node.
	var memoryLimitExceeded atomic.Bool
	stopMonitor := make(chan struct{})
	defer close(stopMonitor)

	if params.limitBytes > 0 {
		var initialMem runtime.MemStats
		runtime.ReadMemStats(&initialMem)
		if initialMem.Alloc > params.limitBytes {
			memoryLimitExceeded.Store(true)
			lg.Warn("Heap memory limit exceeded at start; flagging search termination", zap.Uint64("limit", params.limitBytes), zap.Uint64("allocated", initialMem.Alloc))
		} else {
			go func() {
				ticker := time.NewTicker(monitorInterval)
				defer ticker.Stop()
				for {
					select {
					case <-ticker.C:
						var mem runtime.MemStats
						runtime.ReadMemStats(&mem)
						if mem.Alloc > params.limitBytes {
							// Set the atomic flag. StepContext checks this flag in a thread-safe,
							// lock-free manner on every state transition.
							memoryLimitExceeded.Store(true)
							lg.Warn("Heap memory limit exceeded; flagging search termination", zap.Uint64("limit", params.limitBytes), zap.Uint64("allocated", mem.Alloc))
							return
						}
					case <-stopMonitor:
						return
					}
				}
			}()
		}
	}

	// We wrap the model's StepContext function to pass a context that implements memory limit checking.
	// If the memory limit is breached, Err() on this context will return context.Canceled.
	// Because the non-deterministic model's apply function and state transitions (which implement
	// StepContext) check ctx.Err() != nil, they will abort immediately, returning false, nil.
	originalStepContext := m.StepContext
	m.StepContext = func(ctx context.Context, state interface{}, input interface{}, output interface{}) (bool, interface{}) {
		wrappedCtx := memoryLimitContext{
			Context:       ctx,
			limitExceeded: &memoryLimitExceeded,
		}
		return originalStepContext(wrappedCtx, state, input, output)
	}

	check, info := porcupine.CheckOperationsVerbose(m, params.operations, params.timeout)
	duration := time.Since(start)

	result := LinearizationResult{
		Info:  info,
		Model: m,
	}

	// If the memory limit was tripped, we override the check outcome to report
	// MemoryUsageExceeded. Since the DFS prunes all paths, Porcupine would otherwise
	// report porcupine.Illegal (which is a false positive correctness failure).
	if params.limitBytes > 0 && memoryLimitExceeded.Load() {
		var mem runtime.MemStats
		runtime.ReadMemStats(&mem)
		result.Status = MemoryUsageExceeded
		result.Message = fmt.Sprintf("memory limit exceeded: current heap size %d bytes exceeds limit %d bytes", mem.Alloc, params.limitBytes)
		lg.Error("Linearization aborted due to memory limit", zap.Duration("duration", duration), zap.Uint64("limit", params.limitBytes), zap.Uint64("allocated", mem.Alloc))
		return result
	}

	switch check {
	case porcupine.Ok:
		result.Status = Success
		lg.Info("Linearization success", zap.Duration("duration", duration))
	case porcupine.Unknown:
		result.Status = Timeout
		result.Message = "timed out"
		lg.Error("Linearization timed out", zap.Duration("duration", duration))
	case porcupine.Illegal:
		result.Status = Failure
		result.Message = "illegal"
		lg.Error("Linearization illegal", zap.Duration("duration", duration))
	default:
		result.Status = Failure
		result.Message = fmt.Sprintf("unknown results from porcupine: %s", check)
	}
	return result
}

func validateSerializableOperations(lg *zap.Logger, operations []porcupine.Operation, replay *model.EtcdReplay) Result {
	lg.Info("Validating serializable operations")
	start := time.Now()
	err := validateSerializableOperationsError(lg, operations, replay)
	if err != nil {
		lg.Error("Serializable validation failed", zap.Duration("duration", time.Since(start)), zap.Error(err))
	} else {
		lg.Info("Serializable validation success", zap.Duration("duration", time.Since(start)))
	}
	return ResultFromError(err)
}

func validateSerializableOperationsError(lg *zap.Logger, operations []porcupine.Operation, replay *model.EtcdReplay) (lastErr error) {
	for _, read := range operations {
		request := read.Input.(model.EtcdRequest)
		response := read.Output.(model.MaybeEtcdResponse)
		err := validateSerializableRead(lg, replay, request, response)
		if err != nil {
			lastErr = err
		}
	}
	return lastErr
}

func validateSerializableRead(lg *zap.Logger, replay *model.EtcdReplay, request model.EtcdRequest, response model.MaybeEtcdResponse) error {
	if response.Persisted || response.Error != "" {
		return nil
	}
	state, err := replay.StateForRevision(request.Range.Revision)
	if err != nil {
		if response.Error == model.ErrEtcdFutureRev.Error() {
			return nil
		}
		lg.Error("Failed validating serializable operation", zap.Any("request", request), zap.Any("response", response))
		return errFutureRevRespRequested
	}

	_, expectResp := state.Step(request)

	if diff := cmp.Diff(response.EtcdResponse.Range, expectResp.Range); diff != "" {
		lg.Error("Failed validating serializable operation", zap.Any("request", request), zap.String("diff", diff))
		return errRespNotMatched
	}
	return nil
}

type memoryLimitContext struct {
	context.Context
	limitExceeded *atomic.Bool
}

func (c memoryLimitContext) Err() error {
	if c.limitExceeded.Load() {
		return context.Canceled
	}
	return c.Context.Err()
}

func (c memoryLimitContext) Done() <-chan struct{} {
	if c.limitExceeded.Load() {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return c.Context.Done()
}
