package main

// Transactional Racing Executor Monkey - TREM \o/

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"math/rand"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
	utls "github.com/refraction-networking/utls"
)

type pattern struct {
	re      *regexp.Regexp
	keyword string
}

// LogWriter - interface for worker (my monkeys) output,one mokey per thread :)
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
	conn     net.Conn // if persistent connection (keepalive) is on
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
	mode        string
	monkeys     []*monkey
	readyChan   chan int
	startChan   chan struct{}
	loopChan    chan struct{}
	wg          sync.WaitGroup
	hostFlag    string
	delayMs     int
	outFlag     bool
	keepAlive   bool
	proxyURL    string
	loopStart   int
	loopTimes   int
	quitChan    chan struct{}
}

var app *tview.Application
var pages *tview.Pages

var logo = [3]string{ `
░▒▓████████▓▒░▒▓███████▓▒░░▒▓████████▓▒░▒▓██████████████▓▒░  
   ░▒▓█▓▒░   ░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░      ░▒▓█▓▒░░▒▓█▓▒░░▒▓█▓▒░ 
   ░▒▓█▓▒░   ░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░      ░▒▓█▓▒░░▒▓█▓▒░░▒▓█▓▒░ 
   ░▒▓█▓▒░   ░▒▓███████▓▒░░▒▓██████▓▒░ ░▒▓█▓▒░░▒▓█▓▒░░▒▓█▓▒░ 
   ░▒▓█▓▒░   ░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░      ░▒▓█▓▒░░▒▓█▓▒░░▒▓█▓▒░ 
   ░▒▓█▓▒░   ░▒▓█▓▒░░▒▓█▓▒░▒▓█▓▒░      ░▒▓█▓▒░░▒▓█▓▒░░▒▓█▓▒░ 
   ░▒▓█▓▒░   ░▒▓█▓▒░░▒▓█▓▒░▒▓████████▓▒░▒▓█▓▒░░▒▓█▓▒░░▒▓█▓▒░ 

`,`
▄▄▄█████▓ ██▀███  ▓█████  ███▄ ▄███▓
▓  ██▒ ▓▒▓██ ▒ ██▒▓█   ▀ ▓██▒▀█▀ ██▒
▒ ▓██░ ▒░▓██ ░▄█ ▒▒███   ▓██    ▓██░
░ ▓██▓ ░ ▒██▀▀█▄  ▒▓█  ▄ ▒██    ▒██ 
  ▒██▒ ░ ░██▓ ▒██▒░▒████▒▒██▒   ░██▒
  ▒ ░░   ░ ▒▓ ░▒▓░░░ ▒░ ░░ ▒░   ░  ░
    ░      ░▒ ░ ▒░ ░ ░  ░░  ░      ░
  ░        ░░   ░    ░   ░      ░   
            ░        ░  ░       ░   `,`
░▀█▀░█▀▄░█▀▀░█▄█
░░█░░█▀▄░█▀▀░█░█
░░▀░░▀░▀░▀▀▀░▀░▀
`}
              

func main() {
	hostFlag := flag.String("h", "", "Host:port override; default is addr from Host HTTP Header.")
	listFlag := flag.String("l", "", "Comma-separated request RAW HTTP/1.1 files.")
	reFlag := flag.String("re", "", "Regex (Golang) definitions file. Each line applies to a request file, respectively.\nExamples:\n  One Regex line for each request file, e.g., line 1 will use regexes for request 1\n  Format: regex1':key1$$regex2':key2$$...regex':keyN supports multiples regex per line/request\n  Note: Use backtick (`) character, not (').")
	thrFlag := flag.Int("th", 1, "Threads count.")
	delayFlag := flag.Int("d", 0, "Delay ms between reqs.")
	outFlag := flag.Bool("o", false, "Save *last* (per-thread) HTTP response as out_<timestamp>_t<threadID>.txt")
	univFlag := flag.String("u", "", "Universal replace key=val every request; e.g., !treco!=Val replaces !treco! to Val in every request, multiple times if matched.")
	proxyFlag := flag.String("px", "", "HTTP proxy; http://ip:port")
	modeFlag := flag.String("mode", "async", "Mode: sync or async.")
	kaFlag := flag.Bool("k", false, "Keep-alive connections; persist TLS tunnels for every request, including while looping (-xt N).")
	loopStartFlag := flag.Int("x", -1, "When looping, chain request x to N, where x is -l [1..x..N], default disabled.\n Ex: -l \"req1,req2,req4\" -x 2, does reqs 1 to 4, then iterates -xt times from req2 to req4")
	loopTimesFlag := flag.Int("xt", 0, "Requests loop count:\n 0=infinite\n-1= zero loops")
	flag.Parse()
	
	fmt.Print(logo[rand.Intn(len(logo))]+"\nTransactional Racing Executor Monkey - aka Macaco v0.8 Pu\n\n")
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
	if *loopStartFlag == 0 || (*loopStartFlag == -1 && *loopTimesFlag>1) {
		*loopStartFlag = 1 // treat 0 as 1
	} 

	allPatterns, err := loadPatterns(*reFlag)
	if err != nil {
		exitErr(err.Error())
	}
	if len(allPatterns) != len(reqFiles)-1 {
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
		mode:      *modeFlag,
		monkeys:   monkeys,
		hostFlag:  *hostFlag,
		delayMs:   *delayFlag,
		outFlag:   *outFlag,
		keepAlive: *kaFlag,
		proxyURL:  *proxyFlag,
		loopStart: *loopStartFlag,
		loopTimes: *loopTimesFlag,
		quitChan:  make(chan struct{}),
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
				readyCount++
				if readyCount == len(monkeys) {
					close(orch.startChan)
					break
				}
			}
			
			// Loop orchestration
			if orch.loopStart > 0 {
				loopCount := 0
				for {
					// Check exit conditions
					if orch.loopTimes > 0  && loopCount >= orch.loopTimes {
						break
					}
					
					select {
					case <-orch.quitChan:
						return
					default:
					}
					
					// Reset barrier channels for next iteration
					orch.readyChan = make(chan int, len(monkeys))
					orch.startChan = make(chan struct{})
					
					// Signal all monkeys to start next loop
					close(orch.loopChan)
					orch.loopChan = make(chan struct{})
					
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

		// Establish connection
		w.logger.Write(fmt.Sprintf("Connecting: %s\n", addr))
		conn, err := dialWithProxy(addr, o.proxyURL)
		if err != nil {
			w.logger.Write(fmt.Sprintf("ERR: dial: %v\n", err))
			return
		}

		// TLS handshake if needed
		host, port, _ := net.SplitHostPort(addr)
		if port == "443" {
			tlsConf := &utls.Config{
				InsecureSkipVerify: true,
				ServerName:         host,
			}
			tlsConn := utls.UClient(conn, tlsConf, utls.HelloRandomizedNoALPN)
			if err := tlsConn.Handshake(); err != nil {
				w.logger.Write(fmt.Sprintf("ERR: TLS handshake: %v\n", err))
				conn.Close()
				return
			}
			conn = tlsConn
		}

		if o.keepAlive {
			w.conn = conn
			defer conn.Close()
		}

		// Signal ready
		o.readyChan <- w.id
		w.logger.Write("Ready, waiting for sync...\n")

		// Wait for start signal
		<-o.startChan
		w.logger.Write("Sync start!\n")

		// Send first req
		resp, status, err := sendOnConn([]byte(req), conn)
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
				w.loopStartReq = ""  // will be set in processReq
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
					
					// Send cached loop start req
					resp, status, err := sendOnConn([]byte(w.loopStartReq), w.conn)
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
				w.loopStartReq = ""  // will be set in processReqAsync
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
				
				// Send cached loop start req
				resp, status, err := send([]byte(w.loopStartReq), w.loopStartAddr, o.proxyURL)
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

// processReq - handles single req in chain (reuses connection if keepalive)
func (o *Orch) processReq(w *monkey, idx int, addr string) error {
	var raw []byte
	var err error
	
	// Use cache if available
	if len(w.reqCache) > 0 {
		raw = []byte(w.reqCache[idx])
	} else {
		raw, err = os.ReadFile(w.reqFiles[idx])
		if err != nil {
			return err
		}
	}
	req := string(raw)

	// Apply pattern replacements
	keyCount := countKeys(req)
	if len(w.patterns[idx-1]) != keyCount {
		return fmt.Errorf("file %s: expected %d keys, got %d patterns", w.reqFiles[idx], keyCount, len(w.patterns[idx-1]))
	}

	for _, p := range w.patterns[idx-1] {
		m := p.re.FindStringSubmatch(w.prevResp)
		if m == nil {
			return fmt.Errorf("regex did not match for: %s", p.keyword)
		}
		req = strings.ReplaceAll(req, "$"+p.keyword+"$", m[1])
		w.logger.Write(fmt.Sprintf("Matched %s: %s\n", p.keyword, m[1]))
	}

	// Universal replace
	if w.univKey != "" {
		req = strings.ReplaceAll(req, w.univKey, w.univVal)		
	}

	req = normalizeRequest(req)
	
	// Save loop start req if this is the loop start point
	if o.loopStart > 0 && idx == o.loopStart-1 {
		w.loopStartReq = req
	}

	var resp, status string
	if o.keepAlive && w.conn != nil {
		// Reuse connection
		resp, status, err = sendOnConn([]byte(req), w.conn)
	} else {
		// New connection per req
		resp, status, err = send([]byte(req), addr, o.proxyURL)
	}

	if err != nil {
		return err
	}

	w.logger.Write(fmt.Sprintf("HTTP %s\n", status))
	w.prevResp = resp
	return nil
}

// processReqAsync - async mode: process req with new connection
func (o *Orch) processReqAsync(w *monkey, idx int) error {
	var raw []byte
	var err error
	
	// Use cache if available
	if len(w.reqCache) > 0 {
		raw = []byte(w.reqCache[idx])
	} else {
		raw, err = os.ReadFile(w.reqFiles[idx])
		if err != nil {
			return err
		}
	}
	req := string(raw)

	// Apply patterns if not first req
	if idx > 0 {
		keyCount := countKeys(req)
		if len(w.patterns[idx-1]) != keyCount {
			return fmt.Errorf("file %s: expected %d keys, got %d patterns", w.reqFiles[idx], keyCount, len(w.patterns[idx-1]))
		}

		for _, p := range w.patterns[idx-1] {
			m := p.re.FindStringSubmatch(w.prevResp)
			if m == nil {
				//w.logger.Write(w.prevResp+"\n")
				return fmt.Errorf("regex did not match for: %s", p.keyword)
			}
			req = strings.ReplaceAll(req, "$"+p.keyword+"$", m[1])
			w.logger.Write(fmt.Sprintf("Matched %s: %s\n", p.keyword, m[1]))
		}
	}

	// Universal replace
	if w.univKey != "" {
		req = strings.ReplaceAll(req, w.univKey, w.univVal)
		//w.logger.Write(fmt.Sprintf("Univ %s: %s\n", w.univKey, w.univVal))
	}

	addr, err := parseHost(req, o.hostFlag)
	if err != nil {
		return err
	}

	req = normalizeRequest(req)
	
	// Save loop start req and addr if this is the loop start point
	if o.loopStart > 0 && idx == o.loopStart-1 {
		w.loopStartReq = req
		w.loopStartAddr = addr
	}
	
	w.logger.Write(fmt.Sprintf("Conn: %s\n", addr))

	resp, status, err := send([]byte(req), addr, o.proxyURL)
	if err != nil {
		return err
	}

	w.logger.Write(fmt.Sprintf("HTTP %s\n", status))
	w.prevResp = resp
	return nil
}

func loadPatterns(path string) ([][]pattern, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	patterns := make([][]pattern, len(lines))
	for i, line := range lines {
		// multiple patterns per line separated by <space>$<space>!
		parts := strings.Split(line, " $ ") 
		patterns[i] = make([]pattern, len(parts))		
		for j, part := range parts {			
			bef, aft, found := strings.Cut(part, "`:")
			//fmt.Print(bef+" "+ aft+" "+part+"\n")
			if !found {
				return nil, errors.New("invalid regex line: " + line)
			}
			r, err := regexp.Compile(bef)
			//fmt.Println(r)
			if err != nil {
				return nil, err
			}
			patterns[i][j] = pattern{re: r, keyword: aft}
		}
	}
	return patterns, nil
}

func countKeys(req string) int {
	i := 0
	cnt := make(map[string]int)
	for {
		start := strings.Index(req[i:], "$")
		if start < 0 {
			break
		}
		i += start + 1
		end := strings.Index(req[i:], "$")
		if end < 0 {
			break
		}
		cnt[req[i:i+end]]++
		i += end + 1
	}
	return len(cnt)
}

func normalizeRequest(req string) string {
	lines := strings.Split(req, "\n")
	var normalized []string
	var bodyStartIndex int = -1

	for i, line := range lines {
		line = strings.TrimRight(line, "\r")
		if i == 0 && !strings.Contains(line, "HTTP/") {
			line = line + " HTTP/1.1"
		} else if i == 0 && strings.Contains(line, "HTTP/") && !strings.Contains(line, "HTTP/1.1") {
			re := regexp.MustCompile(`HTTP/\d+\.\d+`)
			line = re.ReplaceAllString(line, "HTTP/1.1")
		}

		normalized = append(normalized, line)

		if line == "" && bodyStartIndex == -1 {
			bodyStartIndex = i + 1
		}
	}

	if bodyStartIndex > 0 && bodyStartIndex < len(normalized) {
		bodyLines := normalized[bodyStartIndex:]
		body := strings.Join(bodyLines, "\r\n")
		bodyLen := len([]byte(body))

		foundContentLength := false
		for i := 1; i < bodyStartIndex-1; i++ {
			if strings.HasPrefix(strings.ToLower(normalized[i]), "content-length:") {
				normalized[i] = fmt.Sprintf("Content-Length: %d", bodyLen)
				foundContentLength = true
				break
			}
		}
		if !foundContentLength && bodyLen > 0 {
			normalized = append(normalized[:bodyStartIndex-1],
				append([]string{fmt.Sprintf("Content-Length: %d", bodyLen)},
					normalized[bodyStartIndex-1:]...)...)
		}
	}

	return strings.Join(normalized, "\r\n")
}

func parseHost(req, override string) (string, error) {
	if override != "" {
		return override, nil
	}

	re := regexp.MustCompile(`(?im)^Host:\s*([^:\r\n]+)(?::(\d+))?`)
	for _, line := range strings.Split(req, "\n") {
		if m := re.FindStringSubmatch(line); m != nil {
			h := m[1]
			port := m[2]
			if port == "" {
				return h + ":443", nil
			}
			return h + ":" + port, nil
		}
	}
	return "", errors.New("no Host header found")
}

func dialWithProxy(addr, proxyURL string) (net.Conn, error) {
	if proxyURL == "" {
		return net.Dial("tcp", addr)
	}

	proxy, err := url.Parse(proxyURL)
	if err != nil {
		return nil, err
	}

	proxyConn, err := net.Dial("tcp", proxy.Host)
	if err != nil {
		return nil, err
	}

	connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", addr, addr)
	_, err = proxyConn.Write([]byte(connectReq))
	if err != nil {
		proxyConn.Close()
		return nil, err
	}

	var response []byte
	buf := make([]byte, 1)
	for {
		_, err := proxyConn.Read(buf)
		if err != nil {
			proxyConn.Close()
			return nil, err
		}
		response = append(response, buf[0])
		if len(response) >= 4 &&
			response[len(response)-4] == '\r' &&
			response[len(response)-3] == '\n' &&
			response[len(response)-2] == '\r' &&
			response[len(response)-1] == '\n' {
			break
		}
	}

	respStr := string(response)
	if !strings.Contains(respStr, "200") {
		proxyConn.Close()
		return nil, fmt.Errorf("proxy CONNECT failed: %s", strings.Split(respStr, "\r\n")[0])
	}

	return proxyConn, nil
}

// send - creates new connection and sends req
func send(raw []byte, addr, proxyURL string) (resp, status string, err error) {
	conn, err := dialWithProxy(addr, proxyURL)
	if err != nil {
		return "", "", err
	}
	defer conn.Close()

	host, port, _ := net.SplitHostPort(addr)

	if port == "443" {
		tlsConf := &utls.Config{
			InsecureSkipVerify: true,
			ServerName:         host,
		}
		tlsConn := utls.UClient(conn, tlsConf, utls.HelloRandomizedNoALPN)
		err = tlsConn.Handshake()
		if err != nil {
			return "", "", err
		}
		conn = tlsConn
	}

	return sendOnConn(raw, conn)
}

// sendOnConn - sends req on existing connection
func sendOnConn(raw []byte, conn net.Conn) (resp, status string, err error) {
	var sb strings.Builder
	var blen, bread int64
	var encod string
	var swErr error
	var decodedBody bytes.Buffer

	_, err = conn.Write(raw)
	if err != nil {
		return "", "", err
	}

	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", "", err
	}

	parts := strings.SplitN(line, " ", 3)
	if len(parts) >= 2 {
		status = parts[1]
	}

	for {
		l, err := reader.ReadString('\n')
		if err != nil {
			return "", "", err
		}
		sb.Write([]byte(l))
		if l == "\r\n" {
			break
		}
		if blen == 0 {
			_, bodylen, found := strings.Cut(l, "Content-Length: ")
			if found {
				blen, err = strconv.ParseInt(strings.Split(bodylen, "\r\n")[0], 10, 16)
			}
		}
		if encod == "" {
			_, encod, _ = strings.Cut(l, "Content-Encoding: ")
			encod = strings.TrimSpace(strings.ToLower(encod))
		}
	}

	buf := make([]byte, 1024)
	body := &bytes.Buffer{}
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			bread += int64(n)
			body.Write(buf[:n])
		}
		if err != nil || bread == blen {
			break
		}
	}

	switch encod {
	case "", "identity":
		decodedBody = *body
	case "gzip":
		bodyPlain, err := gzip.NewReader(body)
		if err != nil {
			return "", "", fmt.Errorf("gzip open err: %v", err)
		}
		defer bodyPlain.Close()
		_, swErr = io.Copy(&decodedBody, bodyPlain)
	case "deflate":
		bodyPlain := flate.NewReader(body)
		defer bodyPlain.Close()
		_, swErr = io.Copy(&decodedBody, bodyPlain)
	default:
		return "", "", fmt.Errorf("encoding not supported: %s", encod)
	}

	if swErr != nil {
		return "", "", fmt.Errorf("decode %s err: %v", encod, swErr)
	}

	sb.Write(decodedBody.Bytes())
	return sb.String(), status, nil
}

func exitErr(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}
