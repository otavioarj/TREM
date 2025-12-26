package main

import (
	"bufio"
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

// ValDist - distributes key=value pairs from FIFO to all threads
type ValDist struct {
	fifoPath  string
	current   atomic.Pointer[map[string]string]
	lastKV    atomic.Pointer[[2]string] // last key=value for display
	hasData   atomic.Bool
	firstData chan struct{}
	done      chan struct{}
	mu        sync.Mutex
}

// NewValDist - creates new distributor
func NewValDist(path string) *ValDist {
	d := &ValDist{
		fifoPath:  path,
		firstData: make(chan struct{}),
		done:      make(chan struct{}),
	}
	empty := make(map[string]string)
	d.current.Store(&empty)
	emptyKV := [2]string{"", ""}
	d.lastKV.Store(&emptyKV)
	return d
}

// NewValDistStatic - creates distributor with static k=v (no FIFO)
func NewValDistStatic(key, val string) *ValDist {
	d := &ValDist{
		firstData: make(chan struct{}),
		done:      make(chan struct{}),
	}
	m := map[string]string{key: val}
	d.current.Store(&m)
	kv := [2]string{key, val}
	d.lastKV.Store(&kv)
	d.hasData.Store(true)
	close(d.firstData)
	return d
}

// EnsureFifo - creates FIFO if not exists
func (d *ValDist) EnsureFifo() error {
	if d.fifoPath == "" {
		return nil
	}

	info, err := os.Stat(d.fifoPath)
	if err == nil {
		// Exists - check if named pipe
		if info.Mode()&os.ModeNamedPipe != 0 {
			return nil
		}
		return os.ErrExist // exists but not a FIFO
	}

	if os.IsNotExist(err) {
		return mkfifo(d.fifoPath, 0600)
	}

	return err
}

// Start - begins reading from FIFO
func (d *ValDist) Start() {
	if d.fifoPath == "" {
		return
	}
	go d.reader()
}

// reader - goroutine that reads FIFO and updates values
func (d *ValDist) reader() {
	fifo, err := os.OpenFile(d.fifoPath, os.O_RDONLY, os.ModeNamedPipe)
	if err != nil {
		return
	}
	defer fifo.Close()

	cur := make(map[string]string)
	scanner := bufio.NewScanner(fifo)

	for scanner.Scan() {
		select {
		case <-d.done:
			return
		default:
		}

		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k == "" {
			continue
		}

		// Update map
		d.mu.Lock()
		cur[k] = v
		snapshot := copyMap(cur)
		d.mu.Unlock()

		d.current.Store(&snapshot)
		kv := [2]string{k, v}
		d.lastKV.Store(&kv)

		// Signal first data
		if !d.hasData.Load() {
			d.hasData.Store(true)
			close(d.firstData)
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

// Get - returns current values, no lock used here :)
//
//	Whom races the racer? \o/
func (d *ValDist) Get() map[string]string {
	p := d.current.Load()
	if p == nil {
		return nil
	}
	return *p
}

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

// Path - returns FIFO path
func (d *ValDist) Path() string {
	return d.fifoPath
}

// copyMap - creates shallow copy of map
func copyMap(m map[string]string) map[string]string {
	c := make(map[string]string, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

// applyVals - replaces all keys in req with values from map
func applyVals(req string, vals map[string]string) string {
	for k, v := range vals {
		req = strings.ReplaceAll(req, k, v)
	}
	return req
}
