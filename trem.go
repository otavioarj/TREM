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
var version = "v1.6.0"

// Global flags
var verbose bool
var configBanner string

// Global stats
var stats *StatsCollector

// pattern - regex extraction
type pattern struct {
	re      *regexp.Regexp
	keyword string
}

// httpVersionRe - matches HTTP/x.x at end of request line
var httpVersionRe = regexp.MustCompile(`HTTP/\d\.\d`)

// LogWriter - Interface for async logging
type LogWriter interface {
	Write(msg string)
}

// monkey - represents single thread
type monkey struct {
	id         int
	groupID    int // group this monkey belongs to
	localID    int // thread ID within group (0-based)
	logger     LogWriter
	conn       net.Conn   // HTTP/1.1 persistent connection
	h2conn     *H2Conn    // HTTP/2 connection wrapper
	connAddr   string     // current connection address
	connMu     sync.Mutex // connection mutex
	prevResp   string
	reqFiles   []string
	reqIndices []int       // which request indices to process (0-based, sorted)
	patterns   [][]pattern // patterns for transitions
	// Response action patterns (read-only per thread, uses absolute indices)
	actionPatterns map[int]*actionPattern
	// Cache for loop optimization
	reqCache      []string
	loopStartReq  string
	loopStartAddr string

	// FIFO value distribution
	valChan     chan map[string]string // receives values from ValDist
	localBuffer map[string][]string    // accumulated values for consumption

	// Static values extracted from regex patterns (keys starting with _)
	staticVals map[string]string
}

// Orch - orchestrator for sync/async modes
type Orch struct {
	mode          string
	monkeys       []*monkey
	readyChan     chan int
	startChan     chan struct{}
	loopChan      chan struct{}
	loopReadyChan chan int // workers signal ready for next loop
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
	fifoBlockSize int      // FIFO block consumption limit
	// Sync barrier
	syncBarriers map[int]bool // request indices that trigger barrier (0-based, relative to group)
	barrierMu    sync.Mutex
	barrierCount int
	// Response action and pause mechanism
	pauseMu      sync.Mutex
	pauseCond    *sync.Cond
	pauseAll     bool
	pauseThreads map[int]bool
	uiManager    *UIManager // reference for pa (printAll) broadcast
	// Group mode fields
	groupID    int   // group identifier (0-based)
	reqIndices []int // request indices for this group (0-based, sorted)
	startDelay int   // delay ms before starting threads
}

func main() {
	thrFlag := flag.Int("thr", 1, "Thread count (single mode only)")
	listFlag := flag.String("l", "", "Request files list, comma separated")
	reFlag := flag.String("re", "", "Regex file: each line N regex from r(N) response that is substituted on r(N+1). Leave line empty for fire-and-forget.")
	modeFlag := flag.String("mode", "async", "Mode: async or sync")
	delayFlag := flag.Int("d", 0, "Delay ms between requests (single mode only)")
	hostFlag := flag.String("h", "", "Host override: host:port")
	outFlag := flag.Bool("o", false, "Output last response to file")
	kaFlag := flag.Bool("k", false, "Keep-alive: reuse TLS connection for chain")
	verboseFlag := flag.Bool("v", false, "Verbose: show debug info")
	proxyFlag := flag.String("px", "", "Proxy URL (socks5://host:port)")
	cliFlag := flag.Int("cli", 0, "uTLS Client Hello ID. 0=Go default, 1=Chrome 124, 2=Firefox 120, 3=Safari 18.0")
	loopStartFlag := flag.Int("x", -1, "Loop from request index (1-based). -1 = no loop")
	loopTimesFlag := flag.Int("xt", 1, "Loop count. 0 = infinite")
	touFlag := flag.Int("tou", 10000, "TLS dial timeout (ms)")
	retryFlag := flag.Int("retry", 3, "Max connection retries")
	httpFlag := flag.Bool("http2", false, "Force HTTP/2")
	univFlag := flag.String("u", "", "Universal: FIFO path or key=value for $_key$ substitution")
	fmodeFlag := flag.Int("fmode", 1, "FIFO distribution mode: 1=round-robin, 2=broadcast")
	fwFlag := flag.Bool("fw", true, "FIFO wait: wait for first value before starting workers")
	mtlsFlag := flag.String("mtls", "", "mTLS client cert: file.p12:password")
	swFlag := flag.Int("sw", 10, "Stats window: sample size for RPS/jitter calculation")
	dumpFlag := flag.Bool("dump", false, "Dump thread logs to files")
	sbFlag := flag.String("sb", "1", "Sync barrier: comma-separated request indices (1-based) for barrier in sync mode.\nExample: -l r1,r2,r3,r5"+
		" -sb 1,2,5 will use barrier for r1, r2 and r5")
	raFlag := flag.String("ra", "", "Response action file. Applies regex on response in a given request indexes (',' separated), and then, execute actions.\nExample: "+
		"-l r1,r2,r3 -ra ra.txt. With ra.txt as:\n  2,3:token.*`:sre(\"/tmp/req.txt\"), e \n"+
		"Will try to match in r2 and r3 responses \"token.*\", then save request that *first* generated the match to \"/tmp/req.txt\" and exit.\n"+
		"The following actions, are implemented:\n pa(\"msg\") - print msg on match and pause ALL threads.\n pt(\"msg\") - print msg on match and pause the thread.\n"+
		" sr  - save request that generated the match, and pause thread.\n sre - save response that generated the match, and pause thread.\n"+
		" sa  - save request and response that generated the match, and pause thread.\n e   - gracefully exit on match, use as last action if combined with others!")
	fbckFlag := flag.Int("fbck", 64, "FIFO block consumption: max values to drain per request (0=unlimited).")
	thrGFlag := flag.String("thrG", "", "Thread groups file. Defines independent request groups with separate threads.\n"+
		"When present, -thr, -mode, -d, -x, -xt, -sb, -re are ignored (defined per group).\n"+
		"Format per line: indices thr=N mode=sync|async delay=N [x=N] [xt=N] [sb=N,M] [re=file]\n"+
		"Example:\n  1,3,5 thr=25 mode=async delay=25 x=1 xt=2\n  2,4 thr=10 mode=sync delay=0 x=2 xt=10 sb=1")
	flag.Parse()

	PrintLogo()
	verbose = *verboseFlag

	// Stats collection window size
	windowSize := *swFlag
	if verbose && windowSize < 50 {
		windowSize = 50
	} else if windowSize < 1 {
		windowSize = 10
	}

	// Check for unexpected positional arguments
	if flag.NArg() > 0 {
		exitErr(fmt.Sprintf("Warning: unexpected argument(s): %v\n", flag.Args()))
	}

	if *listFlag == "" {
		flag.Usage()
		os.Exit(1)
	}

	reqFiles := strings.Split(*listFlag, ",")
	if len(reqFiles) < 2 {
		exitErr("Need at least 2 req files")
	}

	// Load response action patterns (global, uses absolute indices)
	actionPatterns, err := loadActionPatterns(*raFlag)
	if err != nil {
		exitErr(fmt.Sprintf("invalid -ra: %v", err))
	}

	// Branch: Group mode (-thrG) or Single mode (-thr)
	if *thrGFlag != "" {
		runGroupMode(*thrGFlag, reqFiles, actionPatterns, *univFlag, *fmodeFlag,
			*hostFlag, *outFlag, *kaFlag, *proxyFlag, *cliFlag, *touFlag,
			*retryFlag, *httpFlag, *fwFlag, *fbckFlag, *mtlsFlag, *dumpFlag, windowSize)
	} else {
		// Single mode requires -re
		if *reFlag == "" {
			exitErr("-re is required in single mode (or use -thrG for group mode)")
		}
		runSingleMode(reqFiles, *reFlag, *thrFlag, *modeFlag, *delayFlag,
			*loopStartFlag, *loopTimesFlag, *sbFlag, actionPatterns,
			*univFlag, *fmodeFlag, *hostFlag, *outFlag, *kaFlag, *proxyFlag,
			*cliFlag, *touFlag, *retryFlag, *httpFlag, *fwFlag, *fbckFlag,
			*mtlsFlag, *dumpFlag, windowSize)
	}
}

// setupValDist - creates and initializes value distributor
func setupValDist(univFlag string, fmodeFlag, totalThreads int, logger LogWriter) *ValDist {
	if univFlag == "" {
		return NewValDist("", 1, totalThreads)
	}
	if !strings.Contains(univFlag, "=") {
		vd := NewValDist(univFlag, fmodeFlag, totalThreads)
		if err := vd.EnsureFifo(); err != nil {
			exitErr(fmt.Sprintf("FIFO error: %v", err))
		}
		vd.Start(logger)
		return vd
	}
	parts := strings.Split(univFlag, "=")
	if len(parts) != 2 {
		exitErr("Universal must be key=val or file (FIFO) path")
	}
	return NewValDistStatic(parts[0], parts[1])
}

// loadMTLSCert - loads mTLS certificate if specified
func loadMTLSCert(mtlsFlag string) *utls.Certificate {
	if mtlsFlag == "" {
		return nil
	}
	certAndPass := strings.Split(mtlsFlag, ":")
	if len(certAndPass) != 2 {
		exitErr("Client Certificate (mTLS) must be file:pass!")
	}
	cert, err := loadPKCS12Certificate(certAndPass[0], certAndPass[1])
	if err != nil {
		exitErr(err.Error())
	}
	return &cert
}

// runGroupMode - executes TREM with thread groups from -thrG file
func runGroupMode(groupsFile string, reqFiles []string, actionPatterns map[int]*actionPattern,
	univFlag string, fmodeFlag int, hostFlag string, outFlag, kaFlag bool,
	proxyFlag string, cliFlag, touFlag, retryFlag int, httpFlag, fwFlag bool,
	fbckFlag int, mtlsFlag string, dumpFlag bool, windowSize int) {

	// Parse groups file
	groups, err := parseGroupsFile(groupsFile, len(reqFiles))
	if err != nil {
		exitErr(fmt.Sprintf("invalid -thrG: %v", err))
	}

	// Load patterns for each group
	for _, g := range groups {
		numTransitions := len(g.ReqIndices) - 1
		if g.PatternsFile != "" {
			patterns, err := loadPatterns(g.PatternsFile)
			if err != nil {
				exitErr(fmt.Sprintf("group %d patterns: %v", g.ID+1, err))
			}
			if len(patterns) < numTransitions {
				exitErr(fmt.Sprintf("group %d: patterns file needs %d lines, got %d",
					g.ID+1, numTransitions, len(patterns)))
			}
			g.Patterns = patterns[:numTransitions]
		} else {
			// No patterns file = fire-and-forget for all transitions
			g.Patterns = make([][]pattern, numTransitions)
		}
	}

	totalThreads := getTotalThreads(groups)

	// Generate config banner for groups
	configBanner = formatGroupsBanner(groups, univFlag)

	// Init UI with groups
	ui := NewUIManager(totalThreads, dumpFlag)
	ui.SetupGroups(groups)
	ui.Build()

	// Setup value distributor
	valDist := setupValDist(univFlag, fmodeFlag, totalThreads, ui.GetLogger(0))

	// Init stats
	stats = NewStatsCollector(windowSize, verbose)
	stats.SetValDist(valDist)
	ui.StartStatsConsumer(stats.OutputChan())

	// Load mTLS certificate
	tlsCert := loadMTLSCert(mtlsFlag)

	// Create orchestrators for each group
	orchs := make([]*Orch, len(groups))
	var allWg sync.WaitGroup

	for _, g := range groups {
		// Create monkeys for this group
		monkeys := make([]*monkey, g.ThreadCount)
		for i := 0; i < g.ThreadCount; i++ {
			globalID := g.StartThreadID + i
			monkeys[i] = &monkey{
				id:             globalID,
				groupID:        g.ID,
				localID:        i,
				logger:         ui.GetLogger(globalID),
				reqFiles:       reqFiles,
				reqIndices:     g.ReqIndices,
				patterns:       g.Patterns,
				actionPatterns: actionPatterns,
				valChan:        valDist.GetThreadChan(globalID),
				localBuffer:    make(map[string][]string),
				staticVals:     make(map[string]string),
			}
		}

		// Create orchestrator for this group
		orch := &Orch{
			mode:          g.Mode,
			monkeys:       monkeys,
			hostFlag:      hostFlag,
			delayMs:       0,
			outFlag:       outFlag,
			keepAlive:     kaFlag,
			proxyURL:      proxyFlag,
			loopStart:     g.LoopStart,
			loopTimes:     g.LoopTimes,
			quitChan:      make(chan struct{}),
			clientHelloID: cliFlag,
			tlsTimeout:    time.Duration(touFlag) * time.Millisecond,
			tlsCert:       tlsCert,
			maxRetries:    retryFlag,
			httpH2:        httpFlag,
			valDist:       valDist,
			fifoWait:      fwFlag && valDist.IsFifo(),
			fifoBlockSize: fbckFlag,
			syncBarriers:  g.SyncBarriers,
			pauseThreads:  make(map[int]bool),
			uiManager:     ui,
			groupID:       g.ID,
			reqIndices:    g.ReqIndices,
			startDelay:    g.Delay,
		}
		orch.pauseCond = sync.NewCond(&orch.pauseMu)

		if orch.mode == "sync" {
			orch.readyChan = make(chan int, g.ThreadCount)
			orch.startChan = make(chan struct{})
			if orch.loopStart > 0 {
				orch.loopChan = make(chan struct{})
				orch.loopReadyChan = make(chan int, g.ThreadCount)
			}
		}

		orchs[g.ID] = orch
	}

	monkeysFinished := false
	ui.SetupInputCapture(orchs, &monkeysFinished)

	// Start all groups
	for _, orch := range orchs {
		for range orch.monkeys {
			orch.wg.Add(1)
			allWg.Add(1)
		}

		go func(o *Orch) {
			// Wait for FIFO if enabled
			if o.fifoWait {
				stats.SetFifoWaiting(true)
				o.valDist.WaitFirst()
				stats.SetFifoWaiting(false)
			}

			// Apply start delay for this group
			if o.startDelay > 0 {
				time.Sleep(time.Duration(o.startDelay) * time.Millisecond)
			}

			// Start workers
			for _, w := range o.monkeys {
				go func(w *monkey) {
					o.runWorker(w)
					allWg.Done()
				}(w)
			}

			// Run sync orchestration if needed
			if o.mode == "sync" {
				o.runSyncOrchestration()
			}
		}(orch)
	}

	// Wait for all groups to complete
	go func() {
		allWg.Wait()
		monkeysFinished = true
		stats.Stop()
		valDist.Stop()
		ui.BroadcastMessage("\n=== All requests completed ===\n")
		ui.BroadcastMessage("Press Q to quit, Ctrl+N/P:Groups, Tab:Threads\n")
	}()

	if err := ui.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "TView error: %v\n", err)
	}
	defer ui.app.Stop()
}

// runSingleMode - executes TREM with single group (original behavior)
func runSingleMode(reqFiles []string, reFlag string, thrFlag int, modeFlag string,
	delayFlag, loopStartFlag, loopTimesFlag int, sbFlag string,
	actionPatterns map[int]*actionPattern, univFlag string, fmodeFlag int,
	hostFlag string, outFlag, kaFlag bool, proxyFlag string,
	cliFlag, touFlag, retryFlag int, httpFlag, fwFlag bool,
	fbckFlag int, mtlsFlag string, dumpFlag bool, windowSize int) {

	if loopStartFlag > len(reqFiles) {
		exitErr(fmt.Sprintf("-x must be between 1 and %d", len(reqFiles)))
	}
	if loopStartFlag == 0 || (loopStartFlag == -1 && loopTimesFlag > 1) {
		loopStartFlag = 1
	}

	allPatterns, err := loadPatterns(reFlag)
	if err != nil {
		exitErr(err.Error())
	}
	if len(allPatterns) < len(reqFiles)-1 {
		exitErr(fmt.Sprintf("regex file must have %d lines, got %d", len(reqFiles)-1, len(allPatterns)))
	}

	// Parse sync barriers
	var syncBarriers map[int]bool
	if sbFlag != "" {
		syncBarriers, err = parseIndexList(sbFlag)
		if err != nil {
			exitErr(fmt.Sprintf("invalid -sb: %v", err))
		}
	} else {
		syncBarriers = make(map[int]bool)
	}

	configBanner = FormatConfig(thrFlag, delayFlag, loopStartFlag, loopTimesFlag, cliFlag, fbckFlag,
		kaFlag, verbose, httpFlag, proxyFlag, hostFlag, mtlsFlag, modeFlag, univFlag)

	// Build request indices (all requests)
	reqIndices := make([]int, len(reqFiles))
	for i := range reqFiles {
		reqIndices[i] = i
	}

	// Init UI in single mode
	ui := NewUIManager(thrFlag, dumpFlag)
	ui.SetupSingleMode(thrFlag, len(reqFiles))
	ui.Build()

	// Setup value distributor
	valDist := setupValDist(univFlag, fmodeFlag, thrFlag, ui.GetLogger(0))

	stats = NewStatsCollector(windowSize, verbose)
	stats.SetValDist(valDist)
	ui.StartStatsConsumer(stats.OutputChan())

	// Create monkeys
	monkeys := make([]*monkey, thrFlag)
	for i := 0; i < thrFlag; i++ {
		monkeys[i] = &monkey{
			id:             i,
			groupID:        0,
			localID:        i,
			logger:         ui.GetLogger(i),
			reqFiles:       reqFiles,
			reqIndices:     reqIndices,
			patterns:       allPatterns,
			actionPatterns: actionPatterns,
			valChan:        valDist.GetThreadChan(i),
			localBuffer:    make(map[string][]string),
			staticVals:     make(map[string]string),
		}
	}

	// Load mTLS certificate
	tlsCert := loadMTLSCert(mtlsFlag)

	// Create orchestrator
	orch := &Orch{
		mode:          modeFlag,
		monkeys:       monkeys,
		hostFlag:      hostFlag,
		delayMs:       delayFlag,
		outFlag:       outFlag,
		keepAlive:     kaFlag,
		proxyURL:      proxyFlag,
		loopStart:     loopStartFlag,
		loopTimes:     loopTimesFlag,
		quitChan:      make(chan struct{}),
		clientHelloID: cliFlag,
		tlsTimeout:    time.Duration(touFlag) * time.Millisecond,
		tlsCert:       tlsCert,
		maxRetries:    retryFlag,
		httpH2:        httpFlag,
		valDist:       valDist,
		fifoWait:      fwFlag && valDist.IsFifo(),
		syncBarriers:  syncBarriers,
		pauseThreads:  make(map[int]bool),
		uiManager:     ui,
		fifoBlockSize: fbckFlag,
		groupID:       0,
		reqIndices:    reqIndices,
	}
	orch.pauseCond = sync.NewCond(&orch.pauseMu)

	monkeysFinished := false
	ui.SetupInputCapture([]*Orch{orch}, &monkeysFinished)

	if orch.mode == "sync" {
		orch.readyChan = make(chan int, thrFlag)
		orch.startChan = make(chan struct{})
		if orch.loopStart > 0 {
			orch.loopChan = make(chan struct{})
			orch.loopReadyChan = make(chan int, thrFlag)
		}
	}

	for range monkeys {
		orch.wg.Add(1)
	}

	go func() {
		if orch.fifoWait {
			stats.SetFifoWaiting(true)
			orch.valDist.WaitFirst()
			stats.SetFifoWaiting(false)
		}

		for _, w := range orch.monkeys {
			go orch.runWorker(w)
		}

		if orch.mode == "sync" {
			orch.runSyncOrchestration()
		}
	}()

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

// runSyncOrchestration - handles sync barrier logic for a group
func (o *Orch) runSyncOrchestration() {
	readyCount := 0
	for range o.readyChan {
		o.barrierMu.Lock()
		readyCount++
		o.barrierCount = readyCount
		if verbose {
			fmt.Printf("[V] G%d barrier: %d/%d ready\n", o.groupID+1, readyCount, len(o.monkeys))
		}
		if readyCount == len(o.monkeys) {
			o.barrierMu.Unlock()
			close(o.startChan)
			break
		}
		o.barrierMu.Unlock()
	}

	if o.loopStart > 0 {
		loopCount := 0
		for {
			if o.loopTimes > 0 && loopCount >= o.loopTimes {
				if verbose {
					fmt.Printf("[V] G%d loop orchestrator: limit reached\n", o.groupID+1)
				}
				break
			}

			select {
			case <-o.quitChan:
				return
			default:
			}

			if verbose {
				fmt.Printf("[V] G%d loop orchestrator: waiting loopReadyChan\n", o.groupID+1)
			}

			loopReadyCount := 0
			for range o.loopReadyChan {
				loopReadyCount++
				if verbose {
					fmt.Printf("[V] G%d loop orchestrator: loopReady %d/%d\n", o.groupID+1, loopReadyCount, len(o.monkeys))
				}
				if loopReadyCount == len(o.monkeys) {
					break
				}
			}

			if verbose {
				fmt.Printf("[V] G%d loop orchestrator: all ready, swapping channels\n", o.groupID+1)
			}

			o.barrierMu.Lock()
			o.readyChan = make(chan int, len(o.monkeys))
			o.startChan = make(chan struct{})
			oldLoopChan := o.loopChan
			o.loopChan = make(chan struct{})
			o.loopReadyChan = make(chan int, len(o.monkeys))
			o.barrierMu.Unlock()

			close(oldLoopChan)

			if verbose {
				fmt.Printf("[V] G%d loop orchestrator: waiting readyChan (barrier)\n", o.groupID+1)
			}

			readyCount = 0
			for range o.readyChan {
				readyCount++
				if verbose {
					fmt.Printf("[V] G%d loop orchestrator: barrier %d/%d\n", o.groupID+1, readyCount, len(o.monkeys))
				}
				if readyCount == len(o.monkeys) {
					break
				}
			}

			close(o.startChan)
			loopCount++
			if verbose {
				fmt.Printf("[V] G%d loop orchestrator: loop %d complete\n", o.groupID+1, loopCount)
			}
		}
	}
}

// formatGroupsBanner - creates config banner for group mode
func formatGroupsBanner(groups []*ThreadGroup, univFlag string) string {
	var lines []string
	lines = append(lines, fmt.Sprintf("Groups: %d | Total Threads: %d", len(groups), getTotalThreads(groups)))
	lines = append(lines, formatGroupsSummary(groups))
	if univFlag != "" {
		if strings.Contains(univFlag, "=") {
			lines = append(lines, fmt.Sprintf("Univ: %s", univFlag))
		} else {
			lines = append(lines, fmt.Sprintf("FIFO: %s", univFlag))
		}
	}
	return strings.Join(lines, "\n")
}

// runWorker - executes request chain for single monkey (unified sync/async)
func (o *Orch) runWorker(w *monkey) {
	defer o.wg.Done()
	defer o.closeWorkerConn(w)

	// Use reqIndices to determine which requests to process
	reqIndices := w.reqIndices
	if len(reqIndices) == 0 {
		w.logger.Write("ERR: no request indices assigned\n")
		return
	}

	// Load cache if loop enabled (cache all reqFiles, access by absolute index)
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

	// Execute request chain using group's request indices
	for relIdx, absIdx := range reqIndices {
		if err := o.processReq(w, relIdx, absIdx); err != nil {
			w.logger.Write(fmt.Sprintf("ERR: %v\n", err))
			return
		}
		time.Sleep(time.Duration(o.delayMs) * time.Millisecond)
	}

	// Loop handling (loopStart is 1-based, relative to group)
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

			// Sync mode: signal ready for loop, then wait for release
			if o.mode == "sync" {
				o.loopReadyChan <- w.id
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

			// Clear buffers for fresh loop iteration
			for k := range w.localBuffer {
				if len(k) > 1 && k[1] != '_' {
					delete(w.localBuffer, k)
				}
			}
			for k := range w.staticVals {
				delete(w.staticVals, k)
			}

			// Execute loop requests starting from loopStart (relative to group)
			for relIdx := o.loopStart - 1; relIdx < len(reqIndices); relIdx++ {
				absIdx := reqIndices[relIdx]
				if err := o.processReq(w, relIdx, absIdx); err != nil {
					w.logger.Write(fmt.Sprintf("ERR: %v\n", err))
					return
				}
				time.Sleep(time.Duration(o.delayMs) * time.Millisecond)
			}
		}
	}

	if o.outFlag {
		filename := fmt.Sprintf("out_%s_g%d_t%d.txt", time.Now().Format("15:04:05"), w.groupID+1, w.localID+1)
		os.WriteFile(filename, []byte(w.prevResp), 0666)
		w.logger.Write(fmt.Sprintf("Saved: %s\n", filename))
	}
}

// checkPause - checks if thread should pause (called at strategic points)
func (o *Orch) checkPause(w *monkey) {
	o.pauseMu.Lock()
	for o.pauseAll || o.pauseThreads[w.id] {
		w.logger.Write("[PAUSED] Press Enter to resume\n")
		o.pauseCond.Wait()
	}
	o.pauseMu.Unlock()
}

// pauseAllThreads - pauses all threads
func (o *Orch) pauseAllThreads() {
	o.pauseMu.Lock()
	o.pauseAll = true
	o.pauseMu.Unlock()
}

// pauseThread - pauses specific thread
func (o *Orch) pauseThread(id int) {
	o.pauseMu.Lock()
	o.pauseThreads[id] = true
	o.pauseMu.Unlock()
}

// resumeAll - resumes all paused threads
func (o *Orch) resumeAll() {
	o.pauseMu.Lock()
	o.pauseAll = false
	for k := range o.pauseThreads {
		delete(o.pauseThreads, k)
	}
	o.pauseCond.Broadcast()
	o.pauseMu.Unlock()
}

func exitErr(msg string) {
	fmt.Fprintln(os.Stderr, "ERR:", msg)
	os.Exit(1)
}
