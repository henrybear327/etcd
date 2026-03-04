// Copyright 2015 The etcd Authors
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
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/cheggaaa/pb/v3"
	"github.com/spf13/cobra"
	"golang.org/x/time/rate"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/pkg/v3/report"
)

// watchLatencyCmd represents the watch latency command
var watchLatencyCmd = &cobra.Command{
	Use:   "watch-latency",
	Short: "Benchmark watch latency",
	Long: `Benchmarks the latency for watches by measuring
	the latency between writing to a key and receiving the
	associated watch response.`,
	Run: watchLatencyFunc,
}

var (
	watchLPutTotal          int
	watchLPutRate           int
	watchLKeySize           int
	watchLValueSize         int
	watchLStreams           int
	watchLWatchersPerStream int
	watchLPrevKV            bool
	watchLReconnectInterval time.Duration
	watchLWatchFromRev      int64

	watchLBgKeyCount     int
	watchLBgKeySize      int
	watchLReconnectSleep time.Duration
)

func init() {
	RootCmd.AddCommand(watchLatencyCmd)
	watchLatencyCmd.Flags().IntVar(&watchLStreams, "streams", 10, "Total watch streams")
	watchLatencyCmd.Flags().IntVar(&watchLWatchersPerStream, "watchers-per-stream", 10, "Total watchers per stream")
	watchLatencyCmd.Flags().BoolVar(&watchLPrevKV, "prevkv", false, "PrevKV enabled on watch requests")

	watchLatencyCmd.Flags().IntVar(&watchLPutTotal, "put-total", 1000, "Total number of put requests")
	watchLatencyCmd.Flags().IntVar(&watchLPutRate, "put-rate", 100, "Number of keys to put per second")
	watchLatencyCmd.Flags().IntVar(&watchLKeySize, "key-size", 32, "Key size of watch response")
	watchLatencyCmd.Flags().IntVar(&watchLValueSize, "val-size", 32, "Value size of watch response")
	watchLatencyCmd.Flags().DurationVar(&watchLReconnectInterval, "reconnect-interval", 0, "How often each watcher reconnects (0 = disabled)")
	watchLatencyCmd.Flags().Int64Var(&watchLWatchFromRev, "watch-from-rev", 0, "Starting revision for watchers (0 = current)")

	watchLatencyCmd.Flags().IntVar(&watchLBgKeyCount, "bg-key-count", 0, "Number of background keys to pre-populate (0 = disabled)")
	watchLatencyCmd.Flags().IntVar(&watchLBgKeySize, "bg-key-size", 102400, "Size in bytes of each background key's value")
	watchLatencyCmd.Flags().DurationVar(&watchLReconnectSleep, "reconnect-sleep", 0, "Sleep duration between disconnect and reconnect (0 = immediate)")
}

func watchLatencyFunc(cmd *cobra.Command, _ []string) {
	key := string(mustRandBytes(watchLKeySize))
	value := string(mustRandBytes(watchLValueSize))

	// Pre-populate background data if requested
	if watchLBgKeyCount > 0 {
		bgClient := mustCreateConn()
		prePopulateBackground(bgClient)
		bgClient.Close()
	}

	wchs, streams := setupWatchChannels(key)
	putClient := mustCreateConn()

	bar = pb.New(watchLPutTotal * len(wchs))
	bar.Start()

	limiter := rate.NewLimiter(rate.Limit(watchLPutRate), watchLPutRate)

	putTimes := make([]time.Time, watchLPutTotal)
	eventTimes := make([][]time.Time, len(wchs))

	// Channel to collect reconnect latency results
	var reconnectResults []report.Result
	var reconnectMu sync.Mutex

	for i, wch := range wchs {
		eventTimes[i] = make([]time.Time, watchLPutTotal)
		stream := streams[i/watchLWatchersPerStream]
		wg.Go(func() {
			eventCount := 0
			ch := wch
			var lastSeenRev int64
			var reconnectTimer *time.Timer
			if watchLReconnectInterval > 0 {
				reconnectTimer = time.NewTimer(watchLReconnectInterval)
				defer reconnectTimer.Stop()
			}

			for eventCount < watchLPutTotal {
				if reconnectTimer != nil {
					select {
					case resp, ok := <-ch:
						if !ok {
							continue
						}
						for _, ev := range resp.Events {
							if ev.Kv != nil {
								rev := ev.Kv.ModRevision
								if rev <= lastSeenRev {
									continue // skip replayed events
								}
								lastSeenRev = rev
							}
							if eventCount < watchLPutTotal {
								eventTimes[i][eventCount] = time.Now()
								eventCount++
								bar.Increment()
							}
						}
					case <-reconnectTimer.C:
						// Sleep before reconnecting to let revisions accumulate
						if watchLReconnectSleep > 0 {
							time.Sleep(watchLReconnectSleep)
						}
						reconnStart := time.Now()

						rev := lastSeenRev + 1
						if watchLWatchFromRev > 0 {
							rev = watchLWatchFromRev
						}
						opts := []clientv3.OpOption{clientv3.WithRev(rev)}
						if watchLPrevKV {
							opts = append(opts, clientv3.WithPrevKV())
						}

						ch = stream.Watch(context.TODO(), key, opts...)

						// Measure time to first event after reconnect
						if watchLReconnectSleep > 0 {
							select {
							case resp, ok := <-ch:
								if ok && len(resp.Events) > 0 {
									reconnEnd := time.Now()
									reconnectMu.Lock()
									reconnectResults = append(reconnectResults, report.Result{Start: reconnStart, End: reconnEnd})
									reconnectMu.Unlock()
									for _, ev := range resp.Events {
										if ev.Kv != nil {
											rev := ev.Kv.ModRevision
											if rev <= lastSeenRev {
												continue
											}
											lastSeenRev = rev
										}
										if eventCount < watchLPutTotal {
											eventTimes[i][eventCount] = time.Now()
											eventCount++
											bar.Increment()
										}
									}
								}
							}
						}

						reconnectTimer.Reset(watchLReconnectInterval)
					}
				} else {
					resp := <-ch
					for range resp.Events {
						eventTimes[i][eventCount] = time.Now()
						eventCount++
						bar.Increment()
					}
				}
			}
		})
	}

	putReport := newReport(cmd.Name() + "-put")
	putReportResults := putReport.Run()
	watchReport := newReport(cmd.Name() + "-watch")
	watchReportResults := watchReport.Run()
	for i := 0; i < watchLPutTotal; i++ {
		// limit key put as per reqRate
		if err := limiter.Wait(context.TODO()); err != nil {
			break
		}
		start := time.Now()
		if _, err := putClient.Put(context.TODO(), key, value); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to Put for watch latency benchmark: %v\n", err)
			os.Exit(1)
		}
		end := time.Now()
		putReport.Results() <- report.Result{Start: start, End: end}
		putTimes[i] = end
	}
	wg.Wait()
	close(putReport.Results())
	bar.Finish()
	fmt.Printf("\nPut summary:\n%s", <-putReportResults)

	for i := 0; i < len(wchs); i++ {
		for j := 0; j < watchLPutTotal; j++ {
			start := putTimes[j]
			end := eventTimes[i][j]
			if end.Before(start) {
				start = end
			}
			watchReport.Results() <- report.Result{Start: start, End: end}
		}
	}

	close(watchReport.Results())
	fmt.Printf("\nWatch events summary:\n%s", <-watchReportResults)

	// Print reconnect latency report if we have results
	if len(reconnectResults) > 0 {
		reconnReport := newReport(cmd.Name() + "-reconnect")
		reconnReportResults := reconnReport.Run()
		for _, r := range reconnectResults {
			reconnReport.Results() <- r
		}
		close(reconnReport.Results())
		fmt.Printf("\nReconnect summary:\n%s", <-reconnReportResults)
	}
}

func setupWatchChannels(key string) ([]clientv3.WatchChan, []clientv3.Watcher) {
	clients := mustCreateClients(totalClients, totalConns)

	streams := make([]clientv3.Watcher, watchLStreams)
	for i := range streams {
		streams[i] = clientv3.NewWatcher(clients[i%len(clients)])
	}
	opts := []clientv3.OpOption{}
	if watchLPrevKV {
		opts = append(opts, clientv3.WithPrevKV())
	}
	if watchLWatchFromRev > 0 {
		opts = append(opts, clientv3.WithRev(watchLWatchFromRev))
	}

	wchs := make([]clientv3.WatchChan, len(streams)*watchLWatchersPerStream)
	for i := 0; i < len(streams); i++ {
		for j := 0; j < watchLWatchersPerStream; j++ {
			wchs[i*watchLWatchersPerStream+j] = streams[i].Watch(context.TODO(), key, opts...)
		}
	}
	return wchs, streams
}

func prePopulateBackground(client *clientv3.Client) {
	fmt.Printf("Pre-populating %d background keys (%d bytes each)...\n", watchLBgKeyCount, watchLBgKeySize)
	bgValue := string(mustRandBytes(watchLBgKeySize))
	for i := 0; i < watchLBgKeyCount; i++ {
		key := fmt.Sprintf("watchlatbg-%d", i)
		if _, err := client.Put(context.TODO(), key, bgValue); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to pre-populate background key %s: %v\n", key, err)
			os.Exit(1)
		}
		if (i+1)%100 == 0 {
			fmt.Printf("  ...populated %d/%d background keys\n", i+1, watchLBgKeyCount)
		}
	}
	fmt.Println("Background pre-population complete.")
}
