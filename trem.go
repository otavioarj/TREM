package main

// Transactional Racing Executor Monkey - TREM \o/

import (
	"flag"
	"fmt"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
)

// Release :)
var version = "v1.4"

// Verbose mode flag
var verbose bool

// Global stats collector
var stats *StatsCollector

type pattern struct {
	re      *regexp.Regexp
	keyword string
}

// LogWriter - interface for worker output
type LogWriter interface {
	Write(msg string)
}

// monkey - represents single thread
type monkey struct {
	id       int
	logger   LogWriter
	conn     net.Conn   // HTTP/1.1 persistent connection
	h2conn   *H2Conn    // HTTP/2 connection wrapper
	connAddr string     // current connection address
	connMu   sync.Mutex // connection mutex
	prevResp string
	reqFiles []string
	patterns [][]pattern

	// Cache for loop optimization
	reqCache      []string
	loopStartReq  string
	loopStartAddr string
}

// Orch - orchestrator for sync/async modes
type Orch struct {
	mode          string
	monkeys       []*monkey
	readyChan     chan int
	startChan     chan struct{}
	loopChan      chan struct{}
	wg            sync.WaitGroup
	hostFlag      string
	delayMs       int
	outFlag       bool
	keepAlive     bool
	proxyURL      string
	loopStart     int
	loopTimes     int
	quitChan      chan struct{}
	clientHelloID int
	tlsTimeout    time.Duration
	tlsCert       *utls.Certificate
	maxRetries    int
	httpH2        bool // HTTP is HTTP2?
	valDist       *ValDist
	// Sync barrier
	barrierMu    sync.Mutex
	barrierCount int
}

// Pre-compiled regex
var httpVersionRe = regexp.MustCompile(`HTTP/\d+\.\d+`)

// Config summit banner for stats window
var configBanner string

func main() {
	hostFlag := flag.String("h", "", "Host:port override; default from Host header.")
	listFlag := flag.String("l", "", "Comma-separated request RAW HTTP/1.1 files.")
	reFlag := flag.String("re", "", "Regex definitions file. Format: regex`:key per line.")
	thrFlag := flag.Int("th", 5, "Thread count.")
	delayFlag := flag.Int("d", 0, "Delay ms between requests.")
	outFlag := flag.Bool("o", false, "Save last response per thread.")
	univFlag := flag.String("u", "", "Universal replace key=val.")
	proxyFlag := flag.String("px", "", "HTTP proxy; http://ip:port")
	modeFlag := flag.String("mode", "async", "Mode: sync or async.")
	kaFlag := flag.Bool("k", true, "Keep-alive connections.")
	loopStartFlag := flag.Int("x", 1, "Loop start index (1-based).")
	loopTimesFlag := flag.Int("xt", 1, "Loop count (0=infinite).")
	cliFlag := flag.Int("cli", 0, "ClientHello ID: 0=Random, 1=Chrome, 2=Firefox, 3=iOS, 4=Edge, 5=Safari")
	touFlag := flag.Int("tou", 500, "TLS timeout in ms.")
	retryFlag := flag.Int("retry", 3, "Max retries on errors.")
	verboseFlag := flag.Bool("v", false, "Verbose output.")
	swFlag := flag.Int("sw", 0, "Stats window size (0=auto: 10 normal, 50 verbose).")
	httpFlag := flag.Int("http", 1, "HTTP version: 1 or 2.")
	fwFlag := flag.Bool("fw", false, "FIFO wait: block until first value written.")
	flag.Parse()

	configBanner = FormatConfig(*thrFlag, *modeFlag, *delayFlag, *kaFlag,
		*loopStartFlag, *loopTimesFlag, *cliFlag, *touFlag,
		*verboseFlag, *proxyFlag, *hostFlag, *httpFlag)
	verbose = *verboseFlag

	// Stats window size
	windowSize := *swFlag
	if windowSize <= 0 {
		if verbose {
			windowSize = 50
		} else {
			windowSize = 10
		}
	}

	PrintLogo()

	if *listFlag == "" || *reFlag == "" {
		flag.PrintDefaults()
		os.Exit(1)
	}

	reqFiles := strings.Split(*listFlag, ",")
	if len(reqFiles) < 2 {
		exitErr("Need at least 2 req files")
	}

	if *loopStartFlag > len(reqFiles) {
		exitErr(fmt.Sprintf("-x must be between 1 and %d", len(reqFiles)))
	}
	if *loopStartFlag == 0 || (*loopStartFlag == -1 && *loopTimesFlag > 1) {
		*loopStartFlag = 1
	}

	allPatterns, err := loadPatterns(*reFlag)
	if err != nil {
		exitErr(err.Error())
	}
	if len(allPatterns) < len(reqFiles)-1 {
		exitErr(fmt.Sprintf("regex file must have %d lines, got %d", len(reqFiles)-1, len(allPatterns)))
	}

	// Setup value distributor (FIFO or static k=v)
	var valDist *ValDist
	if *univFlag != "" {
		// Check if flag contains =, indicating key=value mode
		if !strings.Contains(*univFlag, "=") {
			valDist = NewValDist(*univFlag)
			if err := valDist.EnsureFifo(); err != nil {
				exitErr(fmt.Sprintf("FIFO error: %v", err))
			}
			valDist.Start()
			// FIFO mode forces infinite loops
			*loopTimesFlag = 0
		} else {
			// Static k=v
			parts := strings.SplitN(*univFlag, "=", 2)
			if len(parts) != 2 {
				exitErr("Universal must be key=val or FIFO path")
			}
			// key=value mode created just one entry for map[string]string
			valDist = NewValDistStatic(parts[0], parts[1])
		}
	} else {
		valDist = NewValDist("") // empty, no replacements
	}
	// Init stats collector
	stats = NewStatsCollector(windowSize, verbose)
	stats.SetValDist(valDist) // for FIFO status display

	// Init UI
	ui := NewUIManager(*thrFlag)
	ui.StartStatsConsumer(stats.OutputChan())

	// Wait for first FIFO value if -fw enabled
	if *fwFlag && valDist.IsFifo() {
		stats.SetFifoWaiting(true)
		valDist.WaitFirst()
		stats.SetFifoWaiting(false)
	}

	// Create monkeys
	monkeys := make([]*monkey, *thrFlag)
	for i := 0; i < *thrFlag; i++ {
		monkeys[i] = &monkey{
			id:       i,
			logger:   ui.GetLogger(i),
			reqFiles: reqFiles,
			patterns: allPatterns,
		}
	}

	// Create orchestrator
	orch := &Orch{
		mode:          *modeFlag,
		monkeys:       monkeys,
		hostFlag:      *hostFlag,
		delayMs:       *delayFlag,
		outFlag:       *outFlag,
		keepAlive:     *kaFlag,
		proxyURL:      *proxyFlag,
		loopStart:     *loopStartFlag,
		loopTimes:     *loopTimesFlag,
		quitChan:      make(chan struct{}),
		clientHelloID: *cliFlag,
		tlsTimeout:    time.Duration(*touFlag) * time.Millisecond,
		maxRetries:    *retryFlag,
		httpH2:        false,
		valDist:       valDist,
	}
	if *httpFlag == 2 {
		orch.httpH2 = true
	}

	monkeysFinished := false
	ui.SetupInputCapture(orch, &monkeysFinished)

	if orch.mode == "sync" {
		orch.readyChan = make(chan int, *thrFlag)
		orch.startChan = make(chan struct{})
		if orch.loopStart > 0 {
			orch.loopChan = make(chan struct{})
		}
	}

	// Start workers
	for _, w := range monkeys {
		orch.wg.Add(1)
		go orch.runWorker(w)
	}

	// Sync barrier orchestration
	if orch.mode == "sync" {
		go func() {
			readyCount := 0
			for range orch.readyChan {
				orch.barrierMu.Lock()
				readyCount++
				orch.barrierCount = readyCount
				if verbose {
					fmt.Printf("[V] barrier: %d/%d ready\n", readyCount, len(monkeys))
				}
				if readyCount == len(monkeys) {
					orch.barrierMu.Unlock()
					close(orch.startChan)
					break
				}
				orch.barrierMu.Unlock()
			}

			if orch.loopStart > 0 {
				loopCount := 0
				for {
					if orch.loopTimes > 0 && loopCount >= orch.loopTimes {
						break
					}

					select {
					case <-orch.quitChan:
						return
					default:
					}

					orch.barrierMu.Lock()
					orch.readyChan = make(chan int, len(monkeys))
					orch.startChan = make(chan struct{})
					oldLoopChan := orch.loopChan
					orch.loopChan = make(chan struct{})
					orch.barrierMu.Unlock()

					close(oldLoopChan)

					readyCount = 0
					for range orch.readyChan {
						readyCount++
						if readyCount == len(monkeys) {
							break
						}
					}

					close(orch.startChan)
					loopCount++
				}
			}
		}()
	}

	// Wait completion
	go func() {
		orch.wg.Wait()
		monkeysFinished = true
		stats.Stop()
		valDist.Stop()
		ui.BroadcastMessage("\n=== All requests completed ===\n")
		ui.BroadcastMessage("Press Q to quit, Tab/Shift+Tab to navigate\n")
	}()

	if err := ui.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "TView error: %v\n", err)
	}
}

// runWorker - executes request chain for single monkey
func (o *Orch) runWorker(w *monkey) {
	defer o.wg.Done()
	defer o.closeWorkerConn(w)

	// Load cache if loop enabled
	if o.loopStart > 0 {
		w.reqCache = make([]string, len(w.reqFiles))
		for i, file := range w.reqFiles {
			raw, err := os.ReadFile(file)
			if err != nil {
				w.logger.Write(fmt.Sprintf("ERR: cache load: %v\n", err))
				return
			}
			w.reqCache[i] = string(raw)
		}
	}

	if o.mode == "sync" {
		raw, err := os.ReadFile(w.reqFiles[0])
		if err != nil {
			w.logger.Write(fmt.Sprintf("ERR: read file: %v\n", err))
			return
		}
		req := normalizeRequest(string(raw))

		// Apply values from FIFO or static
		vals := o.valDist.Get()
		if len(vals) > 0 {
			req = applyVals(req, vals)
			k, v := o.valDist.LastKV()
			w.logger.Write(fmt.Sprintf("Val %s: %s\n", k, v))
		}

		addr, err := parseHost(req, o.hostFlag)
		if err != nil {
			w.logger.Write(fmt.Sprintf("ERR: parse host: %v\n", err))
			return
		}

		w.logger.Write(fmt.Sprintf("Connecting: %s\n", addr))

		// Dial and populate w.conn or w.h2conn based on o.httpVer
		if err := o.dialWithRetry(w, addr); err != nil {
			w.logger.Write(fmt.Sprintf("ERR: dial: %v\n", err))
			return
		}

		o.readyChan <- w.id
		w.logger.Write("Ready, waiting for sync...\n")

		<-o.startChan
		w.logger.Write("Sync start!\n")

		resp, status, err := o.sendWithReconnect(w, []byte(req), addr)
		if err != nil {
			w.logger.Write(fmt.Sprintf("ERR: send: %v\n", err))
			return
		}
		w.logger.Write(fmt.Sprintf("HTTP %s\n", status))
		w.prevResp = resp

		// Close if not keepalive (HTTP/1.1 only, H2 always reuses)
		if !o.keepAlive && !o.httpH2 {
			o.closeWorkerConn(w)
		}

		for i := 1; i < len(w.reqFiles); i++ {
			if o.loopStart > 0 && i == o.loopStart-1 {
				w.loopStartReq = ""
				w.loopStartAddr = addr
			}

			if err := o.processReq(w, i, addr); err != nil {
				w.logger.Write(fmt.Sprintf("ERR: %v\n", err))
				return
			}
			time.Sleep(time.Duration(o.delayMs) * time.Millisecond)
		}

		if o.loopStart > 0 {
			loopCount := 0
			for {
				select {
				case <-o.quitChan:
					return
				case <-o.loopChan:
					w.prevResp = ""
					o.readyChan <- w.id
					<-o.startChan

					loopCount++
					if o.loopTimes == 0 {
						w.logger.Write(fmt.Sprintf("\n[Loop %d]\n", loopCount))
					} else {
						w.logger.Write(fmt.Sprintf("\n[Loop %d/%d]\n", loopCount, o.loopTimes))
					}

					resp, status, err := o.sendWithReconnect(w, []byte(w.loopStartReq), w.loopStartAddr)
					if err != nil {
						w.logger.Write(fmt.Sprintf("ERR: loop send: %v\n", err))
						return
					}
					w.logger.Write(fmt.Sprintf("HTTP %s\n", status))
					w.prevResp = resp

					for i := o.loopStart; i < len(w.reqFiles); i++ {
						if err := o.processReq(w, i, w.loopStartAddr); err != nil {
							w.logger.Write(fmt.Sprintf("ERR: %v\n", err))
							return
						}
						time.Sleep(time.Duration(o.delayMs) * time.Millisecond)
					}
				}
			}
		}
	} else {
		// Async mode
		vals := o.valDist.Get()
		if len(vals) > 0 {
			k, v := o.valDist.LastKV()
			w.logger.Write(fmt.Sprintf("Val %s: %s\n", k, v))
		}
		for i := 0; i < len(w.reqFiles); i++ {
			if o.loopStart > 0 && i == o.loopStart-1 {
				w.loopStartReq = ""
				w.loopStartAddr = ""
			}

			if err := o.processReqAsync(w, i); err != nil {
				w.logger.Write(fmt.Sprintf("ERR: %v\n", err))
				return
			}
			time.Sleep(time.Duration(o.delayMs) * time.Millisecond)
		}

		if o.loopStart > 0 {
			loopCount := 0
			for {
				if o.loopTimes > 0 && loopCount >= o.loopTimes {
					break
				}

				select {
				case <-o.quitChan:
					return
				default:
				}

				loopCount++
				if o.loopTimes == 0 {
					w.logger.Write(fmt.Sprintf("\n[Loop %d]\n", loopCount))
				} else {
					w.logger.Write(fmt.Sprintf("\n[Loop %d/%d]\n", loopCount, o.loopTimes))
				}

				w.prevResp = ""

				resp, status, err := o.sendWithRetry(w, []byte(w.loopStartReq), w.loopStartAddr)
				if err != nil {
					w.logger.Write(fmt.Sprintf("ERR: loop send: %v\n", err))
					return
				}
				w.logger.Write(fmt.Sprintf("HTTP %s\n", status))
				w.prevResp = resp

				for i := o.loopStart; i < len(w.reqFiles); i++ {
					if err := o.processReqAsync(w, i); err != nil {
						w.logger.Write(fmt.Sprintf("ERR: %v\n", err))
						return
					}
					time.Sleep(time.Duration(o.delayMs) * time.Millisecond)
				}
			}
		}
	}

	if o.outFlag {
		filename := fmt.Sprintf("out_%s_t%d.txt", time.Now().Format("15:04:05"), w.id)
		os.WriteFile(filename, []byte(w.prevResp), 0666)
		w.logger.Write(fmt.Sprintf("Saved: %s\n", filename))
	}
}

func exitErr(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}
