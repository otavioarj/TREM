package main

// Transactional Racing Executor Monkey - TREM \o/

import (
	"flag"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	utls "github.com/refraction-networking/utls"
)

// Release :)
var version = "v1.5.0"

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

	// FIFO value distribution
	valChan     chan map[string]string // receives values from ValDist
	localBuffer map[string][]string    // accumulated values for consumption
}

// Orch - orchestrator for sync/async modes: most of this are just flags passed as struct
//
//	avoiding long call params or globals
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
	httpH2        bool     // HTTP is HTTP2?
	valDist       *ValDist // FIFO Value distributor struct
	fifoWait      bool     // wait for first FIFO data before starting workers
	// Sync barrier
	syncBarriers map[int]bool // request indices that trigger barrier (0-based)
	barrierMu    sync.Mutex
	barrierCount int
}

// Pre-compiled regex
var httpVersionRe = regexp.MustCompile(`HTTP/\d+\.\d+`)

// Config summit banner for stats window
var configBanner string

// parseSyncBarriers - parses "-sb 1,2,3" into map[int]bool (0-based indices)
func parseSyncBarriers(sb string) map[int]bool {
	barriers := make(map[int]bool)
	if sb == "" {
		return barriers
	}
	parts := strings.Split(sb, ",")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if idx, err := strconv.Atoi(p); err == nil {
			if idx == 0 {
				idx = 1
			}
			barriers[idx-1] = true // convert to 0-based
		}
	}
	return barriers
}

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s -l req1,...,reqn -re regex [options]\n\nOptions:\n", os.Args[0])
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nNote: boolean flags require -flag or -flag=true/false syntax (e.g., -http2=true, not -http2 true)\n")
	}
	hostFlag := flag.String("h", "", "Host:port override; default from Host header.")
	listFlag := flag.String("l", "", "Comma-separated request RAW HTTP/1.1 files.")
	reFlag := flag.String("re", "", "Regex definitions file. Format: regex`:key per line.")
	thrFlag := flag.Int("th", 5, "Thread count.")
	delayFlag := flag.Int("d", 0, "Delay ms between requests.")
	outFlag := flag.Bool("o", false, "Save last response per thread.")
	univFlag := flag.String("u", "", "Universaly replaces key=val.\nIf key=value, ie., $num$=179, will match/replace every"+
		" $num$ (requests) to 179.\nIf a path. ie., /tmp/fifo, will create a named-pipe where other program can write key=value"+
		"\n|-> Example of a password-spray racer: ./trem <params> -u /tmp/fifo || cat pass-spray.txt > /tmp/fifo\n"+
		"    With pass-spray.txt as: $pass$=21938712\n"+
		"                            32847832\n"+
		"                            32473872\n"+
		"                            ...")
	proxyFlag := flag.String("px", "", "HTTP proxy; http://ip:port")
	modeFlag := flag.String("mode", "async", "Mode: sync or async.")
	kaFlag := flag.Bool("k", true, "Keep-alive connections.")
	loopStartFlag := flag.Int("x", 1, "Loop start index (1-based).")
	loopTimesFlag := flag.Int("xt", 1, "Loop count (0=infinite).")
	cliFlag := flag.Int("cli", 0, "ClientHello ID: 0=Random, 1=Chrome, 2=Firefox, 3=iOS, 4=Edge, 5=Safari")
	touFlag := flag.Int("tou", 500, "TLS timeout in ms.")
	retryFlag := flag.Int("retry", 3, "Max retries on errors.")
	verboseFlag := flag.Bool("v", false, "Verbose output.")
	swFlag := flag.Int("sw", 10, "Stats window size (0=auto: 10 normal, 50 verbose).")
	httpFlag := flag.Bool("http2", false, "Use HTTP2, default false uses HTTP 1.1.")
	fwFlag := flag.Bool("fw", true, "FIFO wait: block until first value is written to the named-pipe.")
	fmodeFlag := flag.Int("fmode", 2, "FIFO mode: \n1. Broadcast, all threads receives same values from FIFO. \n2. Round-robin "+
		"Queue, each value is sent sequentially per thread, i.e, as in a ring.")
	mtlsFlag := flag.String("mtls", "", "Client Certificate (mTLS) for TLS, format /path/file.pk12:pass .")
	dumpFlag := flag.Bool("dump", false, "Dump thread output to files (thr<ID>_<H-M>.txt).")
	sbFlag := flag.String("sb", "1", "Sync barrier: comma-separated request indices (1-based) for synchronization in sync mode.")
	flag.Parse()

	configBanner = FormatConfig(*thrFlag, *delayFlag, *loopStartFlag, *loopTimesFlag, *cliFlag, *touFlag,
		*kaFlag, *verboseFlag, *httpFlag,
		*proxyFlag, *hostFlag, *mtlsFlag, *modeFlag, *univFlag)
	verbose = *verboseFlag

	// Stats collection window size, ie., how many request/packs counted to generated delay/jitter etc.
	windowSize := *swFlag

	if verbose && windowSize < 50 {
		windowSize = 50
	} else if windowSize < 1 {
		windowSize = 10
	}

	PrintLogo()
	// Check for unexpected positional arguments: -flag value instead of -flag=value for bool flags)
	if flag.NArg() > 0 {
		exitErr(fmt.Sprintf("Warning: unexpected argument(s): %v\n", flag.Args()))
	}

	if *listFlag == "" || *reFlag == "" {
		flag.Usage()
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

	// Parse sync barriers
	syncBarriers := parseSyncBarriers(*sbFlag)

	// Init UI
	ui := NewUIManager(*thrFlag, *dumpFlag)

	// Setup value distributor (FIFO or static k=v)
	var valDist *ValDist
	if *univFlag != "" {
		// Check if flag contains =, indicating key=value mode
		if !strings.Contains(*univFlag, "=") {
			valDist = NewValDist(*univFlag, *fmodeFlag, *thrFlag)
			if err := valDist.EnsureFifo(); err != nil {
				exitErr(fmt.Sprintf("FIFO error: %v", err))
			}
			valDist.Start(ui.GetLogger(0))
		} else {
			// Static k=v
			parts := strings.Split(*univFlag, "=")
			if len(parts) != 2 {
				exitErr("Universal must be key=val or file (FIFO) path")
			}
			// key=value mode created just one entry for map[string]string
			valDist = NewValDistStatic(parts[0], parts[1])
		}
	} else {
		valDist = NewValDist("", 1, *thrFlag) // empty, no replacements
	}
	// Init stats collector
	stats = NewStatsCollector(windowSize, verbose)
	stats.SetValDist(valDist) // for FIFO status display
	ui.StartStatsConsumer(stats.OutputChan())

	// Note: FIFO wait (-fw) is handled in worker startup goroutine below
	// This allows UI to render while waiting for first FIFO data

	// Create monkeys
	monkeys := make([]*monkey, *thrFlag)
	for i := 0; i < *thrFlag; i++ {
		monkeys[i] = &monkey{
			id:          i,
			logger:      ui.GetLogger(i),
			reqFiles:    reqFiles,
			patterns:    allPatterns,
			valChan:     valDist.GetThreadChan(i),
			localBuffer: make(map[string][]string),
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
		httpH2:        *httpFlag,
		valDist:       valDist,
		fifoWait:      *fwFlag && valDist.IsFifo(),
		syncBarriers:  syncBarriers,
	}

	if *mtlsFlag != "" {
		certAndPass := strings.Split(*mtlsFlag, ":")
		if len(certAndPass) != 2 {
			exitErr("Client Certificate (mTLS) must be file:pass!")
		}
		cert, err := loadPKCS12Certificate(certAndPass[0], certAndPass[1])
		if err != nil {
			exitErr(err.Error())
		}
		orch.tlsCert = &cert
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

	// Add workers to WaitGroup BEFORE starting goroutine
	// This prevents wg.Wait() from returning immediately
	for range monkeys {
		orch.wg.Add(1)
	}

	// Start workers in goroutine (allows UI to render while waiting for FIFO)
	go func() {
		// Wait for first FIFO value if -fw enabled
		if orch.fifoWait {
			stats.SetFifoWaiting(true)
			orch.valDist.WaitFirst()
			stats.SetFifoWaiting(false)
		}

		// Start workers (wg.Add already done above)
		for _, w := range orch.monkeys {
			go orch.runWorker(w)
		}

		// Sync barrier orchestration
		if orch.mode == "sync" {
			readyCount := 0
			for range orch.readyChan {
				orch.barrierMu.Lock()
				readyCount++
				orch.barrierCount = readyCount
				if verbose {
					fmt.Printf("[V] barrier: %d/%d ready\n", readyCount, len(orch.monkeys))
				}
				if readyCount == len(orch.monkeys) {
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
					orch.readyChan = make(chan int, len(orch.monkeys))
					orch.startChan = make(chan struct{})
					oldLoopChan := orch.loopChan
					orch.loopChan = make(chan struct{})
					orch.barrierMu.Unlock()

					close(oldLoopChan)

					readyCount = 0
					for range orch.readyChan {
						readyCount++
						if readyCount == len(orch.monkeys) {
							break
						}
					}

					close(orch.startChan)
					loopCount++
				}
			}
		}
	}()

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
	defer ui.app.Stop()
}

// runWorker - executes request chain for single monkey (unified sync/async)
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

	// Execute request chain
	for i := 0; i < len(w.reqFiles); i++ {
		if err := o.processReq(w, i); err != nil {
			w.logger.Write(fmt.Sprintf("ERR: %v\n", err))
			return
		}
		time.Sleep(time.Duration(o.delayMs) * time.Millisecond)
	}

	// Loop handling
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

			// Sync mode: wait for loop signal
			if o.mode == "sync" {
				select {
				case <-o.quitChan:
					return
				case <-o.loopChan:
				}
			}

			loopCount++
			if o.loopTimes == 0 {
				w.logger.Write(fmt.Sprintf("\n[Loop %d]\n", loopCount))
			} else {
				w.logger.Write(fmt.Sprintf("\n[Loop %d/%d]\n", loopCount, o.loopTimes))
			}

			w.prevResp = ""

			// Execute loop requests
			for i := o.loopStart - 1; i < len(w.reqFiles); i++ {
				if err := o.processReq(w, i); err != nil {
					w.logger.Write(fmt.Sprintf("ERR: %v\n", err))
					return
				}
				time.Sleep(time.Duration(o.delayMs) * time.Millisecond)
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
