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
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/netip"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jellevandenhooff/gosim"
	"github.com/jellevandenhooff/gosim/gosimruntime"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"go.etcd.io/etcd/pkg/v3/osutil"
	"go.etcd.io/etcd/server/v3/embed"
)

// testCluster encapsulates a 3-node etcd cluster running inside gosim.
type testCluster struct {
	machines  []gosim.Machine
	addrs     []string
	names     []string
	endpoints []string
}

// setupTestCluster creates and starts a 3-node etcd cluster inside gosim.
// It configures the gosim runtime (nanotime step, logging suppression,
// simulation timeout) and waits for the cluster to be ready.
func setupTestCluster(t *testing.T) *testCluster {
	t.Helper()

	// Enable per-read time advancement so every time.Now() call returns a
	// unique, strictly increasing value. This satisfies porcupine's
	// AppendableHistory constraints without any application-level hacks.
	gosimruntime.SetNanotimeStep(1)

	// Suppress standard library logging to prevent metatesting's in-memory
	// log buffer from growing unbounded. The etcd server, gRPC, and raft
	// all produce massive amounts of log output.
	// Our report data uses slog.Info() directly, which is separate.
	log.SetOutput(io.Discard)

	gosim.SetSimulationTimeout(2 * time.Minute)

	c := &testCluster{
		addrs:     []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"},
		names:     []string{"etcd-1", "etcd-2", "etcd-3"},
		endpoints: []string{"10.0.0.1:2379", "10.0.0.2:2379", "10.0.0.3:2379"},
	}

	c.machines = make([]gosim.Machine, len(c.names))
	for i, name := range c.names {
		addr := c.addrs[i]
		delay := time.Duration(i) * 100 * time.Millisecond
		c.machines[i] = gosim.NewMachine(gosim.MachineConfig{
			Label: name,
			Addr:  netip.MustParseAddr(addr),
			MainFunc: func() {
				if delay > 0 {
					time.Sleep(delay)
				}
				runEtcdNode(name, addr)
			},
		})
	}

	// Wait for the cluster to be ready
	time.Sleep(3 * time.Second)
	return c
}

// extractWALs stops all machines and restarts each with a WAL reader function
// that reads persisted WAL entries and reports them via slog. Signal handling
// is a no-op in gosim, so etcdMain blocks forever — this is the only way to
// read WAL data after the cluster has been used.
func (c *testCluster) extractWALs() {
	for _, m := range c.machines {
		m.Stop()
	}
	for i, m := range c.machines {
		name := c.names[i]
		dataDir := name + ".etcd"
		m.SetMainFunc(func() {
			readAndReportWAL(name, dataDir)
		})
		m.Restart()
		m.Wait()
	}
}

// makeZapLogger creates a zap logger controlled by the ETCD_LOG_LEVEL env var.
// When unset, returns a nop logger to avoid massive log output in metatesting's
// in-memory buffer. At "warn" or "error" level, memory is bounded (low-volume).
// At "info" level, acceptable for short debugging sessions only.
// Note: This uses ETCD_LOG_LEVEL (not GOSIM_LOG_LEVEL) to avoid interfering
// with gosim's slog handler, which reads GOSIM_LOG_LEVEL and would suppress
// our INFO-level report messages (gosim_report, gosim_wal_report).
func makeZapLogger() *zap.Logger {
	level := os.Getenv("ETCD_LOG_LEVEL")
	if level == "" {
		return zap.NewNop()
	}
	var zapLevel zapcore.Level
	if err := zapLevel.UnmarshalText([]byte(level)); err != nil {
		return zap.NewNop()
	}
	cfg := zap.NewProductionConfig()
	cfg.Level = zap.NewAtomicLevelAt(zapLevel)
	lg, err := cfg.Build()
	if err != nil {
		return zap.NewNop()
	}
	return lg
}

func parseUrls(a ...string) []url.URL {
	var us []url.URL
	for _, s := range a {
		u, err := url.Parse(s)
		if err != nil {
			panic(err)
		}
		us = append(us, *u)
	}
	return us
}

func runEtcdNode(name, addr string) {
	dataDir := name + ".etcd"
	cfg := embed.NewConfig()
	cfg.Name = name
	cfg.Dir = dataDir
	cfg.QuotaBackendBytes = 8 * 1024 * 1024

	cfg.ListenPeerUrls = parseUrls(fmt.Sprintf("http://%s:2380", addr))
	cfg.AdvertisePeerUrls = parseUrls(fmt.Sprintf("http://%s:2380", addr))
	cfg.ListenClientUrls = parseUrls(fmt.Sprintf("http://%s:2379", addr))
	cfg.AdvertiseClientUrls = parseUrls(fmt.Sprintf("http://%s:2379", addr))

	cfg.InitialCluster = "etcd-1=http://10.0.0.1:2380,etcd-2=http://10.0.0.2:2380,etcd-3=http://10.0.0.3:2380"
	cfg.InitialClusterToken = "toktok"

	// Use "existing" if the data directory already exists (i.e., restart after crash).
	if _, err := os.Stat(dataDir); err == nil {
		cfg.ClusterState = embed.ClusterStateFlagExisting
	} else {
		cfg.ClusterState = embed.ClusterStateFlagNew
	}

	cfg.ZapLoggerBuilder = embed.NewZapLoggerBuilder(makeZapLogger())

	etcdMain(cfg)
}

func etcdMain(cfg *embed.Config) {
	if err := cfg.Validate(); err != nil {
		log.Fatal(err)
	}
	// Skip SetupGlobalLoggers since we use a nop logger and want to
	// avoid gRPC logging to os.Stderr (captured in memory by gosim).
	zap.ReplaceGlobals(cfg.GetLogger())
	lg := cfg.GetLogger()

	e, err := embed.StartEtcd(cfg)
	if err != nil {
		slog.Error("etcd start failed", "error", err.Error())
		return
	}
	osutil.RegisterInterruptHandler(e.Close)
	select {
	case <-e.Server.ReadyNotify():
	case <-e.Server.StopNotify():
	}
	stopped := e.Server.StopNotify()
	errc := e.Err()

	osutil.HandleInterrupts(lg)

	select {
	case lerr := <-errc:
		slog.Error("listener failed", "error", lerr.Error())
	case <-stopped:
	}
}
