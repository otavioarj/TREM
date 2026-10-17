package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Event type flags (bitmask)
const (
	StatReqDone   uint8 = 1 << 0 // request completed
	StatTLSDone   uint8 = 1 << 1 // TLS handshake ok
	StatHTTPError uint8 = 1 << 2 // HTTP error (4xx/5xx)
	StatTLSRetry  uint8 = 1 << 3 // TLS retry attempt
)

// StatEvent - single event struct for quueed metric types (8 bytes)
type StatEvent struct {
	Flags    uint8  // event type bitmask
	ThreadID uint8  // 0-255
	Status   uint16 // HTTP status or TLS attempts
	Latency  uint32 // microseconds
}

// ErrorCount - tracks HTTP errors per thread+status
type ErrorCount struct {
	ThreadID int
	Status   int
	Count    int
}

// statCounts - keeps each worker's totals apart to reduce cache contention
type statCounts struct {
	req atomic.Uint64 // total request count
	in  atomic.Uint64 // bytes received
	out atomic.Uint64 // bytes sent
	_   [104]byte     // 128-byte stride keeps worker counters 
}

// StatsCollector - aggregates metrics from all threads
type StatsCollector struct {
	// Input channel (non-blocking sends)
	eventCh chan StatEvent

	// Config
	windowSize int
	verbose    bool

	// Aggregated state (mutex-protected)
	mu sync.RWMutex

	// Request metrics - separate buffer for req/s (keeps last 2 seconds worth)
	reqTimestamps []time.Time // for req/s calculation, not limited by windowSize

	// Jitter samples (verbose only)
	jitterSamples []float64 // latency samples in ms

	// TLS metrics (verbose only)
	tlsLatencies []float64 // handshake latency samples
	tlsAttempts  []int     // attempts per handshake

	// Per-worker request and I/O totals
	counts []statCounts

	// HTTP errors - map["tid:status"] -> count
	httpErrors map[string]int
	errorOrder []string // insertion order for display

	// Group structure for G#T# display
	groupOffsets []int // starting globalID for each group

	// FIFO status
	valDist     *ValDist
	fifoWaiting atomic.Bool

	// Output
	outputCh chan string
	quitCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once
}

// NewStatsCollector - creates collector with given window size and worker count.
func NewStatsCollector(windowSize int, verbose bool, threads int) *StatsCollector {
	bufSize := windowSize * 4 // buffer for bursts

	sc := &StatsCollector{
		eventCh:       make(chan StatEvent, bufSize),
		counts:        make([]statCounts, threads),
		doneCh:        make(chan struct{}),
		windowSize:    windowSize,
		verbose:       verbose,
		reqTimestamps: make([]time.Time, 0, 128), // separate from windowSize
		httpErrors:    make(map[string]int),
		errorOrder:    make([]string, 0),
		outputCh:      make(chan string, 4),
		quitCh:        make(chan struct{}),
	}

	// Verbose-only allocations
	if verbose {
		sc.jitterSamples = make([]float64, 0, windowSize)
		sc.tlsLatencies = make([]float64, 0, windowSize)
		sc.tlsAttempts = make([]int, 0, windowSize)
	}

	go sc.worker()
	return sc
}

// SetGroupOffsets - configures group structure for G#T# display
// offsets[i] = starting globalID for group i
func (sc *StatsCollector) SetGroupOffsets(offsets []int) {
	sc.mu.Lock()
	sc.groupOffsets = offsets
	sc.mu.Unlock()
}

// globalIDToGroupThread - converts globalID to (groupID, localID)
func (sc *StatsCollector) globalIDToGroupThread(globalID int) (int, int) {
	if len(sc.groupOffsets) == 0 {
		return 0, globalID
	}
	for i := len(sc.groupOffsets) - 1; i >= 0; i-- {
		if globalID >= sc.groupOffsets[i] {
			return i, globalID - sc.groupOffsets[i]
		}
	}
	return 0, globalID
}

// Report - non-blocking send to stats channel
func (sc *StatsCollector) Report(event StatEvent) {
	select {
	case sc.eventCh <- event:
	default:
		// Totals are recorded separately; queued events may be dropped
	}
}

// ReportRequest - records completion totals before queuing history samples
func (sc *StatsCollector) ReportRequest(threadID int, latencyUs uint32, bytesIn, bytesOut uint32) {
	sc.counts[threadID].req.Add(1)
	sc.ReportIO(threadID, bytesIn, bytesOut)
	sc.Report(StatEvent{
		Flags:    StatReqDone,
		ThreadID: uint8(threadID),
		Latency:  latencyUs,
	})
}

// ReportIO - records bytes without a completion event; disabled outside verbose mode (-v)
func (sc *StatsCollector) ReportIO(threadID int, bytesIn, bytesOut uint32) {
	if !sc.verbose {
		return
	}
	c := &sc.counts[threadID]
	if bytesIn != 0 {
		c.in.Add(uint64(bytesIn))
	}
	if bytesOut != 0 {
		c.out.Add(uint64(bytesOut))
	}
}

// totals - reads per-worker counters without blocking request processing
func (sc *StatsCollector) totals() (req, in, out uint64) {
	for i := range sc.counts {
		c := &sc.counts[i]
		req += c.req.Load()
		if sc.verbose {
			in += c.in.Load()
			out += c.out.Load()
		}
	}
	return
}

// ReportTLS - helper for TLS handshake completion
func (sc *StatsCollector) ReportTLS(threadID int, latencyUs uint32, attempts int) {
	sc.Report(StatEvent{
		Flags:    StatTLSDone,
		ThreadID: uint8(threadID),
		Latency:  latencyUs,
		Status:   uint16(attempts),
	})
}

// ReportHTTPError - helper for HTTP errors
func (sc *StatsCollector) ReportHTTPError(threadID int, status int) {
	sc.Report(StatEvent{
		Flags:    StatHTTPError,
		ThreadID: uint8(threadID),
		Status:   uint16(status),
	})
}

// ReportTLSRetry - helper for TLS retry events
func (sc *StatsCollector) ReportTLSRetry(threadID int) {
	sc.Report(StatEvent{
		Flags:    StatTLSRetry,
		ThreadID: uint8(threadID),
	})
}

// OutputChan - returns channel for UI consumption
func (sc *StatsCollector) OutputChan() <-chan string {
	return sc.outputCh
}

// Stop - drains queued events and waits for the final snapshot after producers finish
func (sc *StatsCollector) Stop() {
	sc.stopOnce.Do(func() { close(sc.quitCh) })
	<-sc.doneCh
}

// SetValDist - sets value distributor for FIFO status display
func (sc *StatsCollector) SetValDist(vd *ValDist) {
	sc.mu.Lock()
	sc.valDist = vd
	sc.mu.Unlock()
}

// SetFifoWaiting - sets FIFO waiting state
func (sc *StatsCollector) SetFifoWaiting(waiting bool) {
	sc.fifoWaiting.Store(waiting)
}

// worker - consumes events and updates UI periodically
func (sc *StatsCollector) worker() {
	defer close(sc.doneCh)
	defer close(sc.outputCh)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	ticks := 0

	for {
		select {
		case <-sc.quitCh:
			for {
				select {
				case event := <-sc.eventCh:
					sc.processEvent(event)
				default:
					sc.pushUpdate()
					return
				}
			}
		case event := <-sc.eventCh:
			sc.processEvent(event)
		case <-ticker.C:
			sc.pushUpdate()
			ticks++
			if ticks == 10 { // after 1 second of execution, increase the tick
				ticker.Reset(500 * time.Millisecond)
			}
		}
	}
}

// processEvent - updates aggregated state based on event type
func (sc *StatsCollector) processEvent(e StatEvent) {
	sc.mu.Lock()
	defer sc.mu.Unlock()

	switch {
	case e.Flags&StatReqDone != 0:
		// Track request timestamp for req/s (keep last 2 seconds)
		now := time.Now()
		sc.reqTimestamps = append(sc.reqTimestamps, now)

		// Prune old timestamps (older than 2 seconds) using binary search
		// Timestamps are always in ascending order, so binary search is valid
		// This is O(log n) vs O(n) for the previous linear scan
		cutoff := now.Add(-2 * time.Second)
		idx := sort.Search(len(sc.reqTimestamps), func(i int) bool {
			return sc.reqTimestamps[i].After(cutoff)
		})
		if idx > 0 {
			sc.reqTimestamps = sc.reqTimestamps[idx:]
		}

		// Jitter tracking (verbose only, latency already measured by caller)
		if sc.verbose && e.Latency > 0 {
			latencyMs := float64(e.Latency) / 1000.0
			sc.jitterSamples = append(sc.jitterSamples, latencyMs)
			if len(sc.jitterSamples) > sc.windowSize {
				sc.jitterSamples = sc.jitterSamples[1:]
			}
		}

	case e.Flags&StatTLSDone != 0:
		if sc.verbose {
			latencyMs := float64(e.Latency) / 1000.0
			sc.tlsLatencies = append(sc.tlsLatencies, latencyMs)
			if len(sc.tlsLatencies) > sc.windowSize {
				sc.tlsLatencies = sc.tlsLatencies[1:]
			}

			sc.tlsAttempts = append(sc.tlsAttempts, int(e.Status))
			if len(sc.tlsAttempts) > sc.windowSize {
				sc.tlsAttempts = sc.tlsAttempts[1:]
			}
		}

	case e.Flags&StatHTTPError != 0:
		key := fmt.Sprintf("%d:%d", e.ThreadID, e.Status)
		if sc.httpErrors[key] == 0 {
			sc.errorOrder = append(sc.errorOrder, key)
			// Keep only last N error types
			if len(sc.errorOrder) > sc.windowSize {
				oldKey := sc.errorOrder[0]
				delete(sc.httpErrors, oldKey)
				sc.errorOrder = sc.errorOrder[1:]
			}
		}
		sc.httpErrors[key]++
	}
}

// pushUpdate - formats and sends stats to UI
func (sc *StatsCollector) pushUpdate() {
	reqCount, bytesIn, bytesOut := sc.totals()
	sc.mu.RLock()
	defer sc.mu.RUnlock()

	var lines []string
	// Append config summit banner
	lines = append(lines, configBanner)

	// Univ/FIFO status line (unified)
	if sc.valDist != nil {
		if sc.valDist.IsFifo() {
			// FIFO mode
			if sc.fifoWaiting.Load() {
				lines = append(lines, fmt.Sprintf("Univ.: %s [waiting write...]", sc.valDist.Path()))
			} else {
				k, v := sc.valDist.LastKV()
				if k != "" {
					dispV := v
					if len(dispV) > 20 {
						dispV = dispV[:17] + "..."
					}
					lines = append(lines, fmt.Sprintf("Univ.: %s [%s=%s]", sc.valDist.Path(), k, dispV))
				} else {
					lines = append(lines, fmt.Sprintf("Univ.: %s [no data]", sc.valDist.Path()))
				}
			}
		} else if sc.valDist.HasData() {
			// Static k=v mode
			k, v := sc.valDist.LastKV()
			if k != "" {
				dispV := v
				if len(dispV) > 20 {
					dispV = dispV[:17] + "..."
				}
				lines = append(lines, fmt.Sprintf("Univ.: %s=%s", k, dispV))
			}
		}
	}

	// Line 1: Req/s and total count (normal mode) or with jitter (verbose)
	reqPerSec := sc.calcReqPerSec()
	if sc.verbose {
		avgJitter := sc.calcAvgJitter()
		lines = append(lines, fmt.Sprintf("Req/s: %.2f | Avg Jitter: %.2fms | TotalReq: %d", reqPerSec, avgJitter, reqCount))
	} else {
		lines = append(lines, fmt.Sprintf("Req/s: %.2f | TotalReq: %d", reqPerSec, reqCount))
	}

	// Line 2: HTTP Errors
	errStr := sc.formatErrors()
	if errStr != "" {
		lines = append(lines, fmt.Sprintf("HTTP Errs: %s", errStr))
	}

	// Verbose lines
	if sc.verbose {
		// TLS stats
		if len(sc.tlsLatencies) > 0 {
			avgTLS := avgFloat64(sc.tlsLatencies)
			avgAttempts := avgInt(sc.tlsAttempts)
			lines = append(lines, fmt.Sprintf("TLS: %.2fms avg, %.2f attempts avg", avgTLS, avgAttempts))
		}

		// I/O totals
		lines = append(lines, fmt.Sprintf("I/O Total: %s in, %s out",
			formatBytes(bytesIn), formatBytes(bytesOut)))
	}

	output := strings.Join(lines, "\n")

	// Non-blocking send to UI
	select {
	case sc.outputCh <- output:
	default:
		// The collector is the only sender; replacing an old snapshot leaves room?
		select {
		case <-sc.outputCh:
		default:
		}
		sc.outputCh <- output
	}
}

// calcReqPerSec - requests in last second
func (sc *StatsCollector) calcReqPerSec() float64 {
	if len(sc.reqTimestamps) == 0 {
		return 0
	}

	now := time.Now()
	oneSecAgo := now.Add(-1 * time.Second)
	count := 0

	for i := len(sc.reqTimestamps) - 1; i >= 0; i-- {
		if sc.reqTimestamps[i].After(oneSecAgo) {
			count++
		} else {
			break
		}
	}

	return float64(count)
}

// calcAvgJitter - average latency variance
func (sc *StatsCollector) calcAvgJitter() float64 {
	if len(sc.jitterSamples) < 2 {
		return 0
	}

	// Jitter = average of absolute differences between consecutive samples
	var totalDiff float64
	for i := 1; i < len(sc.jitterSamples); i++ {
		diff := sc.jitterSamples[i] - sc.jitterSamples[i-1]
		if diff < 0 {
			diff = -diff
		}
		totalDiff += diff
	}

	return totalDiff / float64(len(sc.jitterSamples)-1)
}

// formatErrors - formats error map as "G1T1(400)x2, G2T3(500)x1"
func (sc *StatsCollector) formatErrors() string {
	if len(sc.httpErrors) == 0 {
		return ""
	}

	// Sort by thread ID for consistent display
	type errEntry struct {
		globalID int
		groupID  int
		localID  int
		status   int
		count    int
	}
	entries := make([]errEntry, 0, len(sc.httpErrors))

	for key, count := range sc.httpErrors {
		var tid, status int
		fmt.Sscanf(key, "%d:%d", &tid, &status)
		gid, lid := sc.globalIDToGroupThread(tid)
		entries = append(entries, errEntry{tid, gid, lid, status, count})
	}

	sort.Slice(entries, func(i, j int) bool {
		if entries[i].globalID != entries[j].globalID {
			return entries[i].globalID < entries[j].globalID
		}
		return entries[i].status < entries[j].status
	})

	var parts []string
	for _, e := range entries {
		var label string
		if len(sc.groupOffsets) > 1 {
			// Multi-group mode: show G#T#
			label = fmt.Sprintf("G%dT%d", e.groupID+1, e.localID+1)
		} else {
			// Single mode: show T#
			label = fmt.Sprintf("T%d", e.globalID+1)
		}

		if e.count > 1 {
			parts = append(parts, fmt.Sprintf("%s(%d)x%d", label, e.status, e.count))
		} else {
			parts = append(parts, fmt.Sprintf("%s(%d)", label, e.status))
		}
	}

	return strings.Join(parts, ", ")
}

// Helper functions

func avgFloat64(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

func avgInt(vals []int) float64 {
	if len(vals) == 0 {
		return 0
	}
	var sum int
	for _, v := range vals {
		sum += v
	}
	return float64(sum) / float64(len(vals))
}

func formatBytes(b uint64) string {
	const (
		KB = 1024
		MB = 1024 * KB
		GB = 1024 * MB
	)

	switch {
	case b >= GB:
		return fmt.Sprintf("%.2fGB", float64(b)/float64(GB))
	case b >= MB:
		return fmt.Sprintf("%.2fMB", float64(b)/float64(MB))
	case b >= KB:
		return fmt.Sprintf("%.2fKB", float64(b)/float64(KB))
	default:
		return fmt.Sprintf("%dB", b)
	}
}
