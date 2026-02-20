package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const batchSize = 64

// kv - key-value pair for batching
type kv struct {
	k, v string
}

// ValDist - distributes key=value pairs from FIFO to threads
// Supports two modes:
//   - Get mode (popMode=1): broadcast to all threads
//   - Pop mode (popMode=2): round-robin to threads
type ValDist struct {
	fifoPath string
	popMode  int

	// Thread channels - only for threads that use FIFO
	// Protected by mu for concurrent access
	mu          sync.RWMutex
	threadChans map[int]chan map[string]string
	threadIDs   []int // ordered list for round-robin

	// Key subscription model: key -> list of threadIDs that need this key
	// If nil, all threads receive all keys (legacy behavior for single mode)
	keySubscribers map[string][]int
	// Per-key round-robin index for fair distribution among subscribers
	keyRRIndex map[string]*atomic.Uint64

	// Round-robin index for Pop mode (used when keySubscribers is nil)
	rrIndex atomic.Uint64

	// Status tracking
	lastKV    atomic.Pointer[[2]string]
	hasData   atomic.Bool
	firstData chan struct{}
	done      chan struct{}
}

// NewValDist - creates new distributor for FIFO mode
// keySubscriptions: map of threadID -> []keys that thread needs (nil = all threads get all keys)
func NewValDist(path string, popMode int, fifoThreadIDs []int, keySubscriptions map[int][]string) *ValDist {
	d := &ValDist{
		fifoPath:    path,
		popMode:     popMode,
		threadChans: make(map[int]chan map[string]string),
		threadIDs:   make([]int, len(fifoThreadIDs)),
		firstData:   make(chan struct{}),
		done:        make(chan struct{}),
	}

	// Copy thread IDs and create buffered channel for each
	copy(d.threadIDs, fifoThreadIDs)
	for _, id := range fifoThreadIDs {
		d.threadChans[id] = make(chan map[string]string, 1000)
	}

	// Build reverse map: key -> []threadIDs (only if subscriptions provided)
	if keySubscriptions != nil && len(keySubscriptions) > 0 {
		d.keySubscribers = make(map[string][]int)
		d.keyRRIndex = make(map[string]*atomic.Uint64)

		for threadID, keys := range keySubscriptions {
			for _, key := range keys {
				d.keySubscribers[key] = append(d.keySubscribers[key], threadID)
			}
		}

		// Initialize per-key round-robin counters
		for key := range d.keySubscribers {
			d.keyRRIndex[key] = &atomic.Uint64{}
		}
	}

	emptyKV := [2]string{"", ""}
	d.lastKV.Store(&emptyKV)

	if path == "" {
		d.hasData.Store(true)
		close(d.firstData)
	}
	return d
}

// NewValDistStatic - creates distributor with static k=v (no FIFO)
func NewValDistStatic(key, val string) *ValDist {
	d := &ValDist{
		popMode:   2,
		firstData: make(chan struct{}),
		done:      make(chan struct{}),
	}
	kv := [2]string{key, val}
	d.lastKV.Store(&kv)
	d.hasData.Store(true)
	close(d.firstData)
	return d
}

// GetThreadChan - returns channel for specific thread (nil if thread doesn't use FIFO)
func (d *ValDist) GetThreadChan(threadID int) chan map[string]string {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.threadChans == nil {
		return nil
	}
	return d.threadChans[threadID] // returns nil if not in map
}

// RemoveThread - removes thread from round-robin distribution
// Called when a thread finishes execution to prevent blocking the FIFO reader
func (d *ValDist) RemoveThread(threadID int) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// Remove channel from map and drain it
	if ch, exists := d.threadChans[threadID]; exists {
		// Drain remaining values to prevent memory leak
		for len(ch) > 0 {
			<-ch
		}
		close(ch)
		delete(d.threadChans, threadID)
	}

	// Remove from threadIDs slice
	for i, id := range d.threadIDs {
		if id == threadID {
			d.threadIDs = append(d.threadIDs[:i], d.threadIDs[i+1:]...)
			break
		}
	}

	// Remove from all key subscriptions
	if d.keySubscribers != nil {
		for key, subs := range d.keySubscribers {
			for i, id := range subs {
				if id == threadID {
					d.keySubscribers[key] = append(subs[:i], subs[i+1:]...)
					break
				}
			}
		}
	}
}

// EnsureFifo - creates FIFO if not exists
func (d *ValDist) EnsureFifo() error {
	if d.fifoPath == "" {
		return nil
	}

	info, err := os.Stat(d.fifoPath)
	if err == nil {
		if info.Mode()&os.ModeNamedPipe != 0 {
			return nil
		}
		return os.ErrExist
	}

	if os.IsNotExist(err) {
		return mkfifo(d.fifoPath, 0600)
	}

	return err
}

// Start - begins reading from FIFO
func (d *ValDist) Start(logger LogWriter) {
	if d.fifoPath == "" {
		exitErr("FIFO path is NULL?!")
	}
	fifo, err := os.OpenFile(d.fifoPath, os.O_RDWR, os.ModeNamedPipe)
	if err != nil {
		exitErr(fmt.Sprintf("FIFO open error: " + err.Error()))
	}
	go d.reader(fifo, logger)
}

// updateStatus - updates lastKV and signals first data
func (d *ValDist) updateStatus(batch []kv) {
	if len(batch) == 0 {
		return
	}
	last := batch[len(batch)-1]
	kvArr := [2]string{last.k, last.v}
	d.lastKV.Store(&kvArr)

	if !d.hasData.Load() {
		d.hasData.Store(true)
		close(d.firstData)
	}
}

// reader - reads FIFO in batches and dispatches to thread channels
func (d *ValDist) reader(fifo *os.File, logger LogWriter) {
	defer fifo.Close()
	var lastKey string
	buf := make([]byte, 4096)
	var leftover []byte

	for {
		select {
		case <-d.done:
			return
		default:
		}
		// Set read deadline for non-blocking behavior
		fifo.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		n, err := fifo.Read(buf)
		if err != nil {
			if os.IsTimeout(err) {
				continue
			}
			// real read error!
			logger.Write(fmt.Sprintf("Error at FIFO reader: " + err.Error()))
			return
		}

		data := append(leftover, buf[:n]...)
		// Read is non-blocking 4KB b-size, which is them split into 64 lines block
		//  OR what was read until timeout err, this is one batch :)
		leftover = nil
		batch := make([]kv, 0, batchSize)
		for {
			idx := bytes.IndexByte(data, '\n')
			if idx < 0 {
				leftover = data
				break
			}

			line := strings.TrimSpace(string(data[:idx]))
			data = data[idx+1:]
			if line == "" {
				continue
			}

			var k, v string
			if eqIdx := strings.Index(line, "="); eqIdx > 0 {
				k = strings.TrimSpace(line[:eqIdx])
				v = strings.TrimSpace(line[eqIdx+1:])
				lastKey = k
			} else {
				if lastKey == "" {
					continue
				}
				k = lastKey
				v = line
			}

			if k != "" {
				batch = append(batch, kv{k, v})
				// Persist _keys to globalStaticVals immediately for cross-thread access
				if len(k) > 0 && k[0] == '_' {
					globalStaticVals.Store(k, v)
				}
			}
		}

		if len(batch) > 0 {
			d.dispatchBatch(batch)
			d.updateStatus(batch)
		}
	}
}

// dispatchThread - sends kv to thread channel (non-blocking)
// Returns true if sent successfully, false if channel full or closed
func (d *ValDist) dispatchThread(key, value string, thrchan chan map[string]string) bool {
	select {
	case thrchan <- map[string]string{key: value}:
		return true
	case <-d.done:
		return false
	default:
		// Channel full, skip to avoid blocking
		return false
	}
}

// dispatchBatch - sends batch to thread channels based on mode
// Non-blocking: if a channel is full, tries next thread (round-robin)
// or skips that thread (broadcast). Never blocks the reader.
func (d *ValDist) dispatchBatch(batch []kv) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	numThreads := len(d.threadIDs)
	if numThreads == 0 {
		return
	}

	if d.popMode == 2 {
		// Round-robin mode
		for _, item := range batch {
			if len(item.k) > 0 && item.k[0] == '_' {
				// Broadcast _keys to all threads (non-blocking)
				for _, id := range d.threadIDs {
					if ch := d.threadChans[id]; ch != nil {
						d.dispatchThread(item.k, item.v, ch)
					}
				}
			} else if d.keySubscribers != nil {
				// Subscription mode: only send to threads that subscribed to this key
				subscribers := d.keySubscribers[item.k]
				if len(subscribers) == 0 {
					// No one subscribed to this key, skip
					continue
				}

				// Round-robin among subscribers only
				rrIndex := d.keyRRIndex[item.k]
				numSubs := len(subscribers)

				for attempts := 0; attempts < numSubs; attempts++ {
					idx := rrIndex.Add(1) - 1
					subIdx := int(idx % uint64(numSubs))
					threadID := subscribers[subIdx]

					if ch := d.threadChans[threadID]; ch != nil {
						if d.dispatchThread(item.k, item.v, ch) {
							break // success, move to next item
						}
					}
					// Channel full or removed, try next subscriber
				}
				// If all subscriber channels full, value is dropped (better than blocking)
			} else {
				// Legacy mode: round-robin among all threads
				for attempts := 0; attempts < numThreads; attempts++ {
					idx := d.rrIndex.Add(1) - 1
					threadIdx := int(idx % uint64(numThreads))
					id := d.threadIDs[threadIdx]
					if ch := d.threadChans[id]; ch != nil {
						if d.dispatchThread(item.k, item.v, ch) {
							break // success, move to next item
						}
					}
					// Channel full or removed, try next thread
				}
				// If all channels full, value is dropped (better than blocking)
			}
		}
	} else {
		// Broadcast mode: each kv goes to ALL threads (non-blocking)
		for _, item := range batch {
			if d.keySubscribers != nil && len(item.k) > 0 && item.k[0] != '_' {
				// Subscription mode: only broadcast to subscribers
				for _, threadID := range d.keySubscribers[item.k] {
					if ch := d.threadChans[threadID]; ch != nil {
						d.dispatchThread(item.k, item.v, ch)
					}
				}
			} else {
				// Legacy mode or _keys: broadcast to all
				for _, id := range d.threadIDs {
					if ch := d.threadChans[id]; ch != nil {
						d.dispatchThread(item.k, item.v, ch)
					}
				}
			}
		}
	}
}

// Stop - signals reader to stop
func (d *ValDist) Stop() {
	select {
	case <-d.done:
	default:
		close(d.done)
	}
}

// Bunch of functions as one line for simplicity for multiples uses
//  golang will inline it :)

// WaitFirst - blocks until first valid k=v received
func (d *ValDist) WaitFirst() {
	<-d.firstData
}

// HasData - returns true if at least one k=v received
func (d *ValDist) HasData() bool {
	return d.hasData.Load()
}

// LastKV - returns last key=value for display
func (d *ValDist) LastKV() (string, string) {
	kv := d.lastKV.Load()
	if kv == nil {
		return "", ""
	}
	return kv[0], kv[1]
}

// IsFifo - returns true if using FIFO mode
func (d *ValDist) IsFifo() bool {
	return d.fifoPath != ""
}

// IsStatic - returns true if static k=v mode (no FIFO)
func (d *ValDist) IsStatic() bool {
	return d.fifoPath == "" && d.threadChans == nil
}

// StaticKV - returns static k=v if in static mode
func (d *ValDist) StaticKV() (string, string) {
	if !d.IsStatic() {
		return "", ""
	}
	return d.LastKV()
}

// Path - returns FIFO path
func (d *ValDist) Path() string {
	return d.fifoPath
}
