package main

// Transactional Racing Executor Monkey - TREM \o/

import (
	"flag"
	"fmt"
	"math/rand"
	"net"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// Verbose mode flag
var verbose bool

type pattern struct {
	re      *regexp.Regexp
	keyword string
}

// LogWriter - interface for worker (my monkeys) output, one mokey per thread :)
type LogWriter interface {
	Write(msg string)
}

// TVLogWriter - writes to tview.TextView
type TVLogWriter struct {
	tv  *tview.TextView
	app *tview.Application
	mu  sync.Mutex
}

func (t *TVLogWriter) Write(msg string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.app.QueueUpdateDraw(func() {
		fmt.Fprint(t.tv, msg)
	})
}

// monkey - represents single thread
type monkey struct {
	id       int
	logger   LogWriter
	conn     net.Conn   // if persistent connection (keepalive) is on
	connAddr string     // address of current connection for reconnect detection
	connMu   sync.Mutex // mutex for connection operations
	prevResp string
	reqFiles []string
	patterns [][]pattern
	univKey  string
	univVal  string

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
	maxRetries    int
	// Sync barrier with mutex protection
	barrierMu    sync.Mutex
	barrierCount int
}

// Pre-compiled regex for performance :)
var httpVersionRe = regexp.MustCompile(`HTTP/\d+\.\d+`)

var app *tview.Application
var pages *tview.Pages

var logo = [3]string{`
░▒▓████████▓▒░▒▓███████▓▒░░▒▓████████▓▒░▒▓██████████████▓▒░  
   ░▒▓█▓▒░   ░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░      ░▒▓█▓▒░░▒▓█▓▒░░▒▓█▓▒░ 
   ░▒▓█▓▒░   ░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░      ░▒▓█▓▒░░▒▓█▓▒░░▒▓█▓▒░ 
   ░▒▓█▓▒░   ░▒▓███████▓▒░░▒▓██████▓▒░ ░▒▓█▓▒░░▒▓█▓▒░░▒▓█▓▒░ 
   ░▒▓█▓▒░   ░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░      ░▒▓█▓▒░░▒▓█▓▒░░▒▓█▓▒░ 
   ░▒▓█▓▒░   ░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░      ░▒▓█▓▒░░▒▓█▓▒░░▒▓█▓▒░ 
   ░▒▓█▓▒░   ░▒▓█▓▒░░▒▓█▓▒░▒▓████████▓▒░▒▓█▓▒░░▒▓█▓▒░░▒▓█▓▒░ `, `
▄▄▄█████▓ ██▀███  ▓█████  ███▄ ▄███▓
▓  ██▒ ▓▒▓██ ▒ ██▒▓█   ▀ ▓██▒▀█▀ ██▒
▒ ▓██░ ▒░▓██ ░▄█ ▒▒███   ▓██    ▓██░
░ ▓██▓ ░ ▒██▀▀█▄  ▒▓█  ▄ ▒██    ▒██ 
  ▒██▒ ░ ░██▓ ▒██▒░▒████▒▒██▒   ░██▒
  ▒ ░░   ░ ▒▓ ░▒▓░░░ ▒░ ░░ ▒░   ░  ░
    ░      ░▒ ░ ▒░ ░ ░  ░░  ░      ░
  ░        ░░   ░    ░   ░      ░   
            ░        ░  ░       ░   `, `
░▀█▀░█▀▄░█▀▀░█▄█
░░█░░█▀▄░█▀▀░█░█
░░▀░░▀░▀░▀▀▀░▀░▀
`}

func main() {
	hostFlag := flag.String("h", "", "Host:port override; default is addr from Host HTTP Header.")
	listFlag := flag.String("l", "", "Comma-separated request RAW HTTP/1.1 files.")
	reFlag := flag.String("re", "", "Regex (Golang) definitions file. Each line applies to a request file, respectively.\nExamples:\n  One Regex line for each request file, e.g., line 1 will use regexes for request 1\n  Format: regex1':key1$$regex2':key2$$...regex':keyN supports multiples regex per line/request\n  Note: Use backtick character, not (').")
	thrFlag := flag.Int("th", 1, "Threads count.")
	delayFlag := flag.Int("d", 100, "Delay ms between reqs.")
	outFlag := flag.Bool("o", false, "Save *last* (per-thread) HTTP response as out_<timestamp>_t<threadID>.txt")
	univFlag := flag.String("u", "", "Universal replace key=val every request; e.g., !treco!=Val replaces !treco! to Val in every request, multiple times if matched.")
	proxyFlag := flag.String("px", "", "HTTP proxy; http://ip:port")
	modeFlag := flag.String("mode", "async", "Mode: sync or async.")
	kaFlag := flag.Bool("k", false, "Keep-alive connections; persist TLS tunnels for every request, including while looping (-xt N).")
	loopStartFlag := flag.Int("x", -1, "When looping, chain request x to N, where x is -l [1..x..N], default disabled.\n Ex: -l \"req1,req2,req4\" -x 2, does reqs 1 to 4, then iterates -xt times from req2 to req4")
	loopTimesFlag := flag.Int("xt", 0, "Requests loop count:\n 0=infinite\n-1= zero loops")
	cliFlag := flag.Int("cli", 0, "ClientHello ID: 0=RandomNoALPN, 1=Chrome_Auto, 2=Firefox_Auto, 3=iOS_Auto, 4=Edge_Auto, 5=Safari_Auto")
	touFlag := flag.Int("tou", 1000, "TLS handshake timeout in milliseconds")
	retryFlag := flag.Int("retry", 3, "Max retries on connection/TLS errors")
	verboseFlag := flag.Bool("v", false, "Verbose debug output")
	flag.Parse()

	verbose = *verboseFlag

	fmt.Print(logo[rand.Intn(len(logo))] + "\nTransactional Racing Executor Monkey - aka Macaco v1.0\n\n")
	if *listFlag == "" || *reFlag == "" {
		flag.PrintDefaults()
		os.Exit(1)
	}

	reqFiles := strings.Split(*listFlag, ",")
	if len(reqFiles) < 2 {
		exitErr("Need at least 2 req files")
	}

	// Validate loop flags
	if *loopStartFlag > len(reqFiles) {
		exitErr(fmt.Sprintf("-x must be between 1 and %d", len(reqFiles)))
	}
	if *loopStartFlag == 0 || (*loopStartFlag == -1 && *loopTimesFlag > 1) {
		*loopStartFlag = 1 // treat 0 as 1
	}

	allPatterns, err := loadPatterns(*reFlag)
	if err != nil {
		exitErr(err.Error())
	}
	if len(allPatterns) < len(reqFiles)-1 {
		exitErr(fmt.Sprintf("regex file must have %d lines, got %d", len(reqFiles)-1, len(allPatterns)))
	}

	var univKey, univVal string
	if *univFlag != "" {
		parts := strings.Split(*univFlag, "=")
		if len(parts) != 2 {
			exitErr("Universal must be key=val")
		}
		univKey, univVal = parts[0], parts[1]
	}

	// Init TView
	app = tview.NewApplication()
	pages = tview.NewPages()

	// Create monkeys and tabs
	monkeys := make([]*monkey, *thrFlag)
	tabNames := make([]string, *thrFlag)

	for i := 0; i < *thrFlag; i++ {
		tv := tview.NewTextView().
			SetDynamicColors(true).
			SetScrollable(true)

		// Capture tv in local scope for closure
		textView := tv
		tv.SetChangedFunc(func() {
			app.QueueUpdateDraw(func() {
				textView.ScrollToEnd()
			})
		})

		tv.SetBorder(true).SetTitle(fmt.Sprintf(" Thread %d | Tab:Next Shift+Tab:Prev Q:Quit ", i+1))
		logWriter := &TVLogWriter{tv: tv, app: app}
		monkeys[i] = &monkey{
			id:       i,
			logger:   logWriter,
			reqFiles: reqFiles,
			patterns: allPatterns,
			univKey:  univKey,
			univVal:  univVal,
		}

		tabName := fmt.Sprintf("t%d", i+1)
		tabNames[i] = tabName
		pages.AddPage(tabName, tv, true, i == 0)
		fmt.Fprintf(tv, "Thread %d starting...\n", i+1)
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
	}
	// Tab navigation
	currentTab := 0
	monkeysFinished := false
	app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyTab {
			currentTab = (currentTab + 1) % len(tabNames)
			pages.SwitchToPage(tabNames[currentTab])
			return nil
		} else if event.Key() == tcell.KeyBacktab {
			currentTab = (currentTab - 1 + len(tabNames)) % len(tabNames)
			pages.SwitchToPage(tabNames[currentTab])
			return nil
		} else if event.Rune() == 'q' || event.Rune() == 'Q' {
			if monkeysFinished {
				app.Stop()
			} else {
				// Signal quit for infinite loops
				select {
				case <-orch.quitChan:
					// already closed
				default:
					close(orch.quitChan)
				}
				app.Stop()
			}
			return nil
		}
		return event
	})

	// Sync mode has a logic barrier at first TLS tunnel, to sync the request
	if orch.mode == "sync" {
		orch.readyChan = make(chan int, *thrFlag)
		orch.startChan = make(chan struct{})
		if orch.loopStart > 0 {
			orch.loopChan = make(chan struct{})
		}
	}

	// Start monkeys
	for _, w := range monkeys {
		orch.wg.Add(1)
		go orch.runWorker(w)
	}

	// Sync mode: wait for all ready, then broadcast start; thus logic barrier :)
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

			// Loop orchestration
			if orch.loopStart > 0 {
				loopCount := 0
				for {
					// Check exit conditions
					if orch.loopTimes > 0 && loopCount >= orch.loopTimes {
						break
					}

					select {
					case <-orch.quitChan:
						return
					default:
					}

					// Reset barrier channels for next iteration
					orch.barrierMu.Lock()
					orch.readyChan = make(chan int, len(monkeys))
					orch.startChan = make(chan struct{})
					oldLoopChan := orch.loopChan
					orch.loopChan = make(chan struct{})
					orch.barrierMu.Unlock()

					// Signal all monkeys to start next loop
					close(oldLoopChan)

					// Wait for all ready
					readyCount = 0
					for range orch.readyChan {
						readyCount++
						if readyCount == len(monkeys) {
							break
						}
					}

					// Broadcast start
					close(orch.startChan)
					loopCount++
				}
			}
		}()
	}

	// Wait completion in background
	go func() {
		orch.wg.Wait()
		monkeysFinished = true

		// Show completion message in all tabs
		for _, w := range monkeys {
			w.logger.Write("\n=== All requests completed ===\n")
			w.logger.Write("Press Q to quit, Tab/Shift+Tab to navigate\n")
		}
	}()

	// Start UI - this blocks until app.Stop()
	if err := app.SetRoot(pages, true).EnableMouse(true).Run(); err != nil {
		fmt.Fprintf(os.Stderr, "TView error: %v\n", err)
	}
}

// runWorker - executes req chain for single monkey
func (o *Orch) runWorker(w *monkey) {
	defer o.wg.Done()
	defer o.closeWorkerConn(w)

	// Load file cache if loop enabled
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

	// Phase 1: Prepare first req + connection (sync mode)
	if o.mode == "sync" {
		// Parse first req
		raw, err := os.ReadFile(w.reqFiles[0])
		if err != nil {
			w.logger.Write(fmt.Sprintf("ERR: read file: %v\n", err))
			return
		}
		req := normalizeRequest(string(raw))

		// Apply universal replace
		if w.univKey != "" {
			req = strings.ReplaceAll(req, w.univKey, w.univVal)
			w.logger.Write(fmt.Sprintf("Univ %s: %s\n", w.univKey, w.univVal))
		}

		// Parse host
		addr, err := parseHost(req, o.hostFlag)
		if err != nil {
			w.logger.Write(fmt.Sprintf("ERR: parse host: %v\n", err))
			return
		}

		// Establish connection with retry
		w.logger.Write(fmt.Sprintf("Connecting: %s\n", addr))
		conn, err := o.dialWithRetry(w, addr)
		if err != nil {
			w.logger.Write(fmt.Sprintf("ERR: dial: %v\n", err))
			return
		}

		if o.keepAlive {
			w.connMu.Lock()
			w.conn = conn
			w.connAddr = addr
			w.connMu.Unlock()
		}

		// Signal ready
		o.readyChan <- w.id
		w.logger.Write("Ready, waiting for sync...\n")

		// Wait for start signal
		<-o.startChan
		w.logger.Write("Sync start!\n")

		// Send first req with reconnect on error
		resp, status, err := o.sendWithReconnect(w, []byte(req), conn, addr)
		if err != nil {
			w.logger.Write(fmt.Sprintf("ERR: send: %v\n", err))
			if !o.keepAlive {
				conn.Close()
			}
			return
		}
		w.logger.Write(fmt.Sprintf("HTTP %s\n", status))
		w.prevResp = resp

		if !o.keepAlive {
			conn.Close()
		}

		// Continue chain from req2
		for i := 1; i < len(w.reqFiles); i++ {
			// Save loop start point
			if o.loopStart > 0 && i == o.loopStart-1 {
				w.loopStartReq = "" // will be set in processReq
				w.loopStartAddr = addr
			}

			if err := o.processReq(w, i, addr); err != nil {
				w.logger.Write(fmt.Sprintf("ERR: %v\n", err))
				return
			}
			time.Sleep(time.Duration(o.delayMs) * time.Millisecond)
		}

		// Execute loops if enabled
		if o.loopStart > 0 {
			loopCount := 0
			for {
				select {
				case <-o.quitChan:
					return
				case <-o.loopChan:
					// Reset prevResp for clean loop
					w.prevResp = ""

					// Wait for barrier
					o.readyChan <- w.id
					<-o.startChan

					loopCount++
					if o.loopTimes == 0 {
						w.logger.Write(fmt.Sprintf("\n[Loop %d]\n", loopCount))
					} else {
						w.logger.Write(fmt.Sprintf("\n[Loop %d/%d]\n", loopCount, o.loopTimes))
					}

					// Send cached loop start req with reconnect
					w.connMu.Lock()
					conn := w.conn
					w.connMu.Unlock()

					resp, status, err := o.sendWithReconnect(w, []byte(w.loopStartReq), conn, w.loopStartAddr)
					if err != nil {
						w.logger.Write(fmt.Sprintf("ERR: loop send: %v\n", err))
						return
					}
					w.logger.Write(fmt.Sprintf("HTTP %s\n", status))
					w.prevResp = resp

					// Continue from loopStart+1 to end
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
		// Async mode: process all reqs sequentially
		w.logger.Write(fmt.Sprintf("Univ %s: %s\n", w.univKey, w.univVal))
		for i := 0; i < len(w.reqFiles); i++ {
			// Save loop start point
			if o.loopStart > 0 && i == o.loopStart-1 {
				w.loopStartReq = "" // will be set in processReqAsync
				w.loopStartAddr = ""
			}

			if err := o.processReqAsync(w, i); err != nil {
				w.logger.Write(fmt.Sprintf("ERR: %v\n", err))
				return
			}
			time.Sleep(time.Duration(o.delayMs) * time.Millisecond)
		}

		// Execute loops if enabled
		if o.loopStart > 0 {
			loopCount := 0
			for {
				// Check exit conditions
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

				// Reset prevResp for clean loop
				w.prevResp = ""

				// Send cached loop start req with retry
				resp, status, err := o.sendWithRetry(w, []byte(w.loopStartReq), w.loopStartAddr)
				if err != nil {
					w.logger.Write(fmt.Sprintf("ERR: loop send: %v\n", err))
					return
				}
				w.logger.Write(fmt.Sprintf("HTTP %s\n", status))
				w.prevResp = resp

				// Continue from loopStart+1 to end
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

	// Save output if requested
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
