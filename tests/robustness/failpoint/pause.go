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

package failpoint

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"

	"go.etcd.io/etcd/tests/v3/framework/e2e"
	"go.etcd.io/etcd/tests/v3/robustness/identity"
	"go.etcd.io/etcd/tests/v3/robustness/report"
	"go.etcd.io/etcd/tests/v3/robustness/traffic"
)

var PauseLeader Failpoint = pauseLeaderFailpoint{duration: 2 * time.Second}

type pauseLeaderFailpoint struct {
	duration time.Duration
}

func (f pauseLeaderFailpoint) Inject(ctx context.Context, t *testing.T, lg *zap.Logger, clus *e2e.EtcdProcessCluster, baseTime time.Time, ids identity.Provider) ([]report.ClientReport, error) {
	leaderIdx := clus.WaitLeader(t)
	leader := clus.Procs[leaderIdx]

	lg.Info("Pausing leader", zap.String("member", leader.Config().Name), zap.Duration("duration", f.duration))
	err := leader.Pause()
	if err != nil {
		return nil, err
	}
	time.Sleep(f.duration)
	lg.Info("Resuming leader", zap.String("member", leader.Config().Name))
	err = leader.Resume()
	if err != nil {
		return nil, err
	}
	return nil, nil
}

func (f pauseLeaderFailpoint) Name() string {
	return "PauseLeader"
}

func (f pauseLeaderFailpoint) Available(config e2e.EtcdProcessClusterConfig, _ e2e.EtcdProcess, _ traffic.Profile) bool {
	return config.ClusterSize > 1
}

var PauseLeaderRepeatedly Failpoint = repeatPauseLeaderFailpoint{
	pauseDuration:  time.Second,
	resumeDuration: time.Second,
	cycles:         5,
}

type repeatPauseLeaderFailpoint struct {
	pauseDuration  time.Duration
	resumeDuration time.Duration
	cycles         int
}

func (f repeatPauseLeaderFailpoint) Inject(ctx context.Context, t *testing.T, lg *zap.Logger, clus *e2e.EtcdProcessCluster, baseTime time.Time, ids identity.Provider) ([]report.ClientReport, error) {
	for i := 0; i < f.cycles; i++ {
		leaderIdx := clus.WaitLeader(t)
		leader := clus.Procs[leaderIdx]

		lg.Info("Pausing leader", zap.Int("cycle", i+1), zap.Int("totalCycles", f.cycles), zap.String("member", leader.Config().Name), zap.Duration("duration", f.pauseDuration))
		if err := leader.Pause(); err != nil {
			return nil, err
		}
		time.Sleep(f.pauseDuration)
		lg.Info("Resuming leader", zap.Int("cycle", i+1), zap.Int("totalCycles", f.cycles), zap.String("member", leader.Config().Name))
		if err := leader.Resume(); err != nil {
			return nil, err
		}
		if i < f.cycles-1 {
			time.Sleep(f.resumeDuration)
		}
	}
	return nil, nil
}

func (f repeatPauseLeaderFailpoint) Name() string {
	return "PauseLeaderRepeatedly"
}

func (f repeatPauseLeaderFailpoint) Available(config e2e.EtcdProcessClusterConfig, _ e2e.EtcdProcess, _ traffic.Profile) bool {
	return config.ClusterSize > 1
}
