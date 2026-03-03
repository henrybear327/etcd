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

package cmd

import (
	"bufio"
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/time/rate"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/pkg/v3/report"
)

var watchLatencySimCmd = &cobra.Command{
	Use:   "watch-latency-sim",
	Short: "Benchmark watch latency with buildFarmSim-style reconnection pattern",
	Long: `Simulates a Kubernetes build farm traffic pattern to benchmark watch
latency and memory behavior during watcher reconnections.

This reproduces the traffic pattern from buildFarmSim that triggers memory
regression in etcd 3.6's kvsToEvents event reuse/caching. The benchmark
creates prefix watchers that periodically disconnect and reconnect (catching
up from a saved revision), while background PUTs generate events that
accumulate during the disconnect gap.`,
	Run: watchLatencySimFunc,
}

var (
	simNamespaces        int
	simJobsPerNS         int
	simJobSize           int
	simPodSize           int
	simEventSize         int
	simSecretsPerNS      int
	simSecretSize        int
	simWatchBuckets      int
	simControllers       int
	simWatchersPerCtrl   int
	simPrevKV            bool
	simReconnectInterval time.Duration
	simReconnectSleep    time.Duration
	simWatchStartDelay   time.Duration
	simPutRate           int
	simRunDuration       time.Duration
	simMetadataIter      int
	simMetricsInterval   time.Duration
	simMetricsOutput     string
)

func init() {
	RootCmd.AddCommand(watchLatencySimCmd)

	watchLatencySimCmd.Flags().IntVar(&simNamespaces, "namespaces", 10, "Number of namespaces")
	watchLatencySimCmd.Flags().IntVar(&simJobsPerNS, "jobs-per-ns", 10, "Jobs per namespace")
	watchLatencySimCmd.Flags().IntVar(&simJobSize, "job-size", 21504, "Job value size in bytes")
	watchLatencySimCmd.Flags().IntVar(&simPodSize, "pod-size", 10240, "Pod value size in bytes")
	watchLatencySimCmd.Flags().IntVar(&simEventSize, "event-size", 1024, "Event value size in bytes")

	watchLatencySimCmd.Flags().IntVar(&simSecretsPerNS, "secrets-per-ns", 50, "Secrets per namespace for DB inflation")
	watchLatencySimCmd.Flags().IntVar(&simSecretSize, "secret-size", 102400, "Secret value size in bytes")

	watchLatencySimCmd.Flags().IntVar(&simWatchBuckets, "watch-buckets", 32, "Number of watch prefix buckets")
	watchLatencySimCmd.Flags().IntVar(&simControllers, "controllers", 3, "Number of simulated controllers")
	watchLatencySimCmd.Flags().IntVar(&simWatchersPerCtrl, "watchers-per-ctrl", 6, "Watchers per controller")
	watchLatencySimCmd.Flags().BoolVar(&simPrevKV, "prevkv", true, "Enable WithPrevKV on watches")

	watchLatencySimCmd.Flags().DurationVar(&simReconnectInterval, "reconnect-interval", 2*time.Minute, "How long a watcher runs before disconnecting")
	watchLatencySimCmd.Flags().DurationVar(&simReconnectSleep, "reconnect-sleep", 30*time.Second, "Sleep between disconnect and reconnect")
	watchLatencySimCmd.Flags().DurationVar(&simWatchStartDelay, "watch-start-delay", 0, "Delay before starting watchers")

	watchLatencySimCmd.Flags().IntVar(&simPutRate, "put-rate", 500, "Max puts per second")
	watchLatencySimCmd.Flags().DurationVar(&simRunDuration, "run-duration", 3*time.Minute+30*time.Second, "Total run duration")
	watchLatencySimCmd.Flags().IntVar(&simMetadataIter, "metadata-iterations", 2, "Metadata patch iterations per job")

	watchLatencySimCmd.Flags().DurationVar(&simMetricsInterval, "metrics-interval", 5*time.Second, "How often to scrape etcd metrics")
	watchLatencySimCmd.Flags().StringVar(&simMetricsOutput, "metrics-output", "", "Path to write metrics CSV (empty = no CSV)")
}

func watchLatencySimFunc(cmd *cobra.Command, _ []string) {
	ctx, cancel := context.WithTimeout(context.Background(), simRunDuration)
	defer cancel()

	putClient := mustCreateConn()
	limiter := rate.NewLimiter(rate.Limit(simPutRate), simPutRate)
	startTime := time.Now()

	var totalWatchEvents atomic.Int64
	var phase atomic.Value
	phase.Store("init")

	// Start metrics collection.
	var snapshots []simMetricsSnapshot
	var snapshotsMu sync.Mutex
	metricsCtx, metricsCancel := context.WithCancel(context.Background())
	defer metricsCancel()
	go collectSimMetrics(metricsCtx, &phase, &snapshots, &snapshotsMu)

	// Start watchers.
	phase.Store("watchers")
	if simWatchStartDelay > 0 {
		fmt.Fprintf(os.Stderr, "Delaying watcher start by %v...\n", simWatchStartDelay)
		time.Sleep(simWatchStartDelay)
	}
	numWatchers := simControllers * simWatchersPerCtrl
	var watchWg sync.WaitGroup
	for i := range numWatchers {
		bucket := i % simWatchBuckets
		prefix := fmt.Sprintf("/benchmark/watcher-%d/", bucket)
		client := mustCreateConn()
		watchWg.Go(func() {
			runSimWatcher(ctx, client, prefix, &totalWatchEvents)
		})
	}
	fmt.Fprintf(os.Stderr, "Started %d watchers across %d buckets\n", numWatchers, simWatchBuckets)

	// Phase 1: Create secrets (outside watcher prefixes to inflate DB).
	phase.Store("secrets")
	putReport := newReport("watch-latency-sim-put")
	putReportResults := putReport.Run()

	secretVal := string(mustRandBytes(simSecretSize))
	totalSecrets := simNamespaces * simSecretsPerNS
	fmt.Fprintf(os.Stderr, "Creating %d secrets (%d ns x %d secrets, %d bytes each)...\n",
		totalSecrets, simNamespaces, simSecretsPerNS, simSecretSize)
secretLoop:
	for ns := range simNamespaces {
		for s := range simSecretsPerNS {
			if err := limiter.Wait(ctx); err != nil {
				break secretLoop
			}
			key := fmt.Sprintf("/benchmark/ns-%d/secret-%d", ns, s)
			start := time.Now()
			if _, err := putClient.Put(ctx, key, secretVal); err != nil {
				if ctx.Err() == nil {
					fmt.Fprintf(os.Stderr, "PUT secret failed: %v\n", err)
				}
				continue
			}
			putReport.Results() <- report.Result{Start: start, End: time.Now()}
		}
	}
	fmt.Fprintf(os.Stderr, "Secrets creation complete\n")

	// Phase 2: Job lifecycles (under watcher prefixes to trigger watch events).
	phase.Store("jobs")
	jobVal := string(mustRandBytes(simJobSize))
	podVal := string(mustRandBytes(simPodSize))
	eventVal := string(mustRandBytes(simEventSize))
	totalJobs := simNamespaces * simJobsPerNS
	putsPerJob := 4 + simMetadataIter + 4 + 3
	fmt.Fprintf(os.Stderr, "Running %d jobs (%d PUTs each, %d total PUTs)...\n",
		totalJobs, putsPerJob, totalJobs*putsPerJob)
jobLoop:
	for ns := range simNamespaces {
		for j := range simJobsPerNS {
			if ctx.Err() != nil {
				break jobLoop
			}
			bucket := (ns*simJobsPerNS + j) % simWatchBuckets
			runSimJobLifecycle(ctx, putClient, limiter, putReport,
				bucket, ns, j, jobVal, podVal, eventVal)
		}
	}
	fmt.Fprintf(os.Stderr, "Job lifecycles complete\n")

	// Phase 3: Hold until run-duration expires (watcher reconnections continue).
	phase.Store("hold")
	remaining := time.Until(startTime.Add(simRunDuration))
	if remaining > 0 {
		fmt.Fprintf(os.Stderr, "Holding for %v (watcher reconnections continue)...\n",
			remaining.Round(time.Second))
	}
	<-ctx.Done()

	// Cleanup.
	phase.Store("done")
	watchWg.Wait()
	metricsCancel()
	close(putReport.Results())

	// Print reports.
	elapsed := time.Since(startTime)
	events := totalWatchEvents.Load()
	fmt.Printf("\nPUT latency summary:\n%s", <-putReportResults)
	fmt.Printf("\nWatch events summary:\n")
	fmt.Printf("  Total events received: %d\n", events)
	if elapsed.Seconds() > 0 {
		fmt.Printf("  Throughput: %.1f events/sec\n", float64(events)/elapsed.Seconds())
	}
	fmt.Printf("  Duration: %v\n", elapsed.Round(time.Millisecond))

	snapshotsMu.Lock()
	printSimMetricsSummary(snapshots)
	if simMetricsOutput != "" {
		writeSimMetricsCSV(simMetricsOutput, snapshots)
	}
	snapshotsMu.Unlock()
}

func runSimWatcher(ctx context.Context, client *clientv3.Client, prefix string, eventCounter *atomic.Int64) {
	var lastRev int64
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		opts := []clientv3.OpOption{clientv3.WithPrefix()}
		if simPrevKV {
			opts = append(opts, clientv3.WithPrevKV())
		}
		if lastRev > 0 {
			opts = append(opts, clientv3.WithRev(lastRev+1))
		}

		watchCtx, watchCancel := context.WithTimeout(ctx, simReconnectInterval)
		watchCh := client.Watch(watchCtx, prefix, opts...)

		for resp := range watchCh {
			if resp.Err() != nil {
				break
			}
			eventCounter.Add(int64(len(resp.Events)))
			if resp.Header.Revision > lastRev {
				lastRev = resp.Header.Revision
			}
		}
		watchCancel()

		// Sleep between disconnect and reconnect; events accumulate during this gap.
		select {
		case <-ctx.Done():
			return
		case <-time.After(simReconnectSleep):
		}
	}
}

func runSimJobLifecycle(ctx context.Context, client *clientv3.Client, limiter *rate.Limiter,
	r report.Report, bucket, ns, job int, jobVal, podVal, eventVal string) {
	base := fmt.Sprintf("/benchmark/watcher-%d/ns-%d", bucket, ns)
	jobKey := fmt.Sprintf("%s/job-%d", base, job)
	podKey := fmt.Sprintf("%s/pod-%d", base, job)

	// 13 PUTs per job (with default metadata-iterations=2):
	//   job: created, pending, running, complete (4)
	//   pod: created, scheduled, running, succeeded (4)
	//   events: scheduled, started, completed (3)
	//   metadata patches (2)
	puts := []struct{ key, val string }{
		{jobKey, jobVal},
		{jobKey, jobVal},
		{podKey, podVal},
		{podKey, podVal},
		{fmt.Sprintf("%s/event-%d-scheduled", base, job), eventVal},
		{podKey, podVal},
		{fmt.Sprintf("%s/event-%d-started", base, job), eventVal},
		{jobKey, jobVal},
		{podKey, podVal},
		{fmt.Sprintf("%s/event-%d-completed", base, job), eventVal},
		{jobKey, jobVal},
	}
	for range simMetadataIter {
		puts = append(puts, struct{ key, val string }{jobKey, jobVal})
	}

	for _, p := range puts {
		if err := limiter.Wait(ctx); err != nil {
			return
		}
		start := time.Now()
		if _, err := client.Put(ctx, p.key, p.val); err != nil {
			if ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "PUT failed: %v\n", err)
			}
			return
		}
		r.Results() <- report.Result{Start: start, End: time.Now()}
	}
}

// Metrics collection

type simMetricsSnapshot struct {
	Timestamp time.Time
	Phase     string
	RSS       float64
	DBSize    float64
	DBInUse   float64
	Watchers  float64
	Events    float64
}

const simMiB = 1024 * 1024

func collectSimMetrics(ctx context.Context, phase *atomic.Value,
	snapshots *[]simMetricsSnapshot, mu *sync.Mutex) {
	httpClient := newSimMetricsHTTPClient()
	ticker := time.NewTicker(simMetricsInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			snap, err := scrapeSimMetrics(httpClient, endpoints[0])
			if err != nil {
				continue
			}
			snap.Phase = phase.Load().(string)
			mu.Lock()
			*snapshots = append(*snapshots, snap)
			mu.Unlock()
			fmt.Fprintf(os.Stderr, "[metrics] phase=%-10s RSS=%7.1fMiB DB=%7.1fMiB watchers=%.0f\n",
				snap.Phase, snap.RSS/simMiB, snap.DBSize/simMiB, snap.Watchers)
		}
	}
}

func scrapeSimMetrics(httpClient *http.Client, endpoint string) (simMetricsSnapshot, error) {
	scheme := "http"
	if !tls.Empty() || tls.TrustedCAFile != "" {
		scheme = "https"
	}

	resp, err := httpClient.Get(fmt.Sprintf("%s://%s/metrics", scheme, endpoint))
	if err != nil {
		return simMetricsSnapshot{}, err
	}
	defer resp.Body.Close()

	snap := simMetricsSnapshot{Timestamp: time.Now()}
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		name := fields[0]
		if idx := strings.IndexByte(name, '{'); idx >= 0 {
			name = name[:idx]
		}
		val, err := strconv.ParseFloat(fields[1], 64)
		if err != nil {
			continue
		}
		switch name {
		case "process_resident_memory_bytes":
			snap.RSS = val
		case "etcd_mvcc_db_total_size_in_bytes":
			snap.DBSize = val
		case "etcd_mvcc_db_total_size_in_use_in_bytes":
			snap.DBInUse = val
		case "etcd_debugging_mvcc_watcher_total":
			snap.Watchers = val
		case "etcd_debugging_mvcc_events_total":
			snap.Events = val
		}
	}
	return snap, scanner.Err()
}

func newSimMetricsHTTPClient() *http.Client {
	if !tls.Empty() || tls.TrustedCAFile != "" {
		tlsConfig, err := tls.ClientConfig()
		if err != nil {
			fmt.Fprintf(os.Stderr, "bad tls config for metrics scraping: %v\n", err)
			return &http.Client{Timeout: 5 * time.Second}
		}
		return &http.Client{
			Timeout:   5 * time.Second,
			Transport: &http.Transport{TLSClientConfig: tlsConfig},
		}
	}
	return &http.Client{Timeout: 5 * time.Second}
}

func printSimMetricsSummary(snapshots []simMetricsSnapshot) {
	if len(snapshots) == 0 {
		fmt.Printf("\nMetrics: no snapshots collected (is etcd exposing /metrics?)\n")
		return
	}

	first := snapshots[0]
	last := snapshots[len(snapshots)-1]
	var peakRSS, peakDB float64
	for _, s := range snapshots {
		if s.RSS > peakRSS {
			peakRSS = s.RSS
		}
		if s.DBSize > peakDB {
			peakDB = s.DBSize
		}
	}

	fmt.Printf("\nMetrics summary (%d samples over %v):\n",
		len(snapshots), last.Timestamp.Sub(first.Timestamp).Round(time.Second))
	fmt.Printf("  RSS  - Initial: %7.1f MiB, Peak: %7.1f MiB, Final: %7.1f MiB",
		first.RSS/simMiB, peakRSS/simMiB, last.RSS/simMiB)
	if first.RSS > 0 {
		fmt.Printf(", Growth: %+.1f MiB (%+.0f%%)",
			(last.RSS-first.RSS)/simMiB, (last.RSS-first.RSS)/first.RSS*100)
	}
	fmt.Println()
	fmt.Printf("  DB   - Initial: %7.1f MiB, Peak: %7.1f MiB, Final: %7.1f MiB\n",
		first.DBSize/simMiB, peakDB/simMiB, last.DBSize/simMiB)
	fmt.Printf("  DB in-use      : %7.1f MiB (final)\n", last.DBInUse/simMiB)
}

func writeSimMetricsCSV(path string, snapshots []simMetricsSnapshot) {
	f, err := os.Create(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create metrics CSV %s: %v\n", path, err)
		return
	}
	defer f.Close()

	w := csv.NewWriter(f)
	defer w.Flush()

	w.Write([]string{"timestamp", "phase", "rss_bytes", "db_size_bytes", "db_in_use_bytes", "watchers", "events"})
	for _, s := range snapshots {
		w.Write([]string{
			s.Timestamp.Format(time.RFC3339),
			s.Phase,
			strconv.FormatFloat(s.RSS, 'f', 0, 64),
			strconv.FormatFloat(s.DBSize, 'f', 0, 64),
			strconv.FormatFloat(s.DBInUse, 'f', 0, 64),
			strconv.FormatFloat(s.Watchers, 'f', 0, 64),
			strconv.FormatFloat(s.Events, 'f', 0, 64),
		})
	}
	fmt.Fprintf(os.Stderr, "Metrics CSV written to %s\n", path)
}
