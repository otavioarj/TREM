package main

import (
	"fmt"
	"math/rand"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/rivo/tview"
)

// Logo variants
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

const (
	logBufferSize = 256 // buffer per thread channel
)

// AsyncLogWriter - non-blocking log writer using buffered channel
type AsyncLogWriter struct {
	ch     chan string
	closed bool
}

// Write - non-blocking send to channel
func (a *AsyncLogWriter) Write(msg string) {
	if a.closed {
		return
	}
	select {
	case a.ch <- msg:
	default:
		// buffer full, drop oldest and retry
		select {
		case <-a.ch:
		default:
		}
		select {
		case a.ch <- msg:
		default:
		}
	}
}

// Close - signal end of logging
func (a *AsyncLogWriter) Close() {
	a.closed = true
	close(a.ch)
}

// UIManager - manages tview app, thread tabs, and stats panel
type UIManager struct {
	app          *tview.Application
	pages        *tview.Pages
	statsPanel   *tview.TextView
	mainFlex     *tview.Flex
	tabNames     []string
	loggers      []*AsyncLogWriter
	textViews    []*tview.TextView
	dumpEnabled  bool
	dumpFiles    []*os.File
	dumpChans    []chan string
	dumpClosed   bool // flag to prevent send on closed channel
	dumpClosedMu sync.Mutex
}

// NewUIManager - creates UI with thread tabs and stats panel
func NewUIManager(threadCount int, dumpEnabled bool) *UIManager {
	ui := &UIManager{
		app:         tview.NewApplication(),
		pages:       tview.NewPages(),
		tabNames:    make([]string, threadCount),
		loggers:     make([]*AsyncLogWriter, threadCount),
		textViews:   make([]*tview.TextView, threadCount),
		dumpEnabled: dumpEnabled,
	}

	if dumpEnabled {
		ui.dumpFiles = make([]*os.File, threadCount)
		ui.dumpChans = make([]chan string, threadCount)
	}

	// Create thread tabs
	for i := 0; i < threadCount; i++ {
		ui.createThreadTab(i)
	}

	// Create stats panel (bottom 1/3)
	ui.statsPanel = tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(false)
	ui.statsPanel.SetBorder(true).SetTitle(" Stats ")

	// Layout: pages (2/3) + stats (1/3) vertical
	ui.mainFlex = tview.NewFlex().
		SetDirection(tview.FlexRow).
		AddItem(ui.pages, 0, 2, true).      // 2/3 height
		AddItem(ui.statsPanel, 0, 1, false) // 1/3 height

	return ui
}

// createThreadTab - creates tab with TextView and AsyncLogWriter
func (ui *UIManager) createThreadTab(idx int) {
	tv := tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true)

	tv.SetBorder(true).SetTitle(fmt.Sprintf(" Thread %d | Tab:Next Shift+Tab:Prev Q:Quit ", idx+1))

	// Async logger with buffered channel
	logger := &AsyncLogWriter{
		ch: make(chan string, logBufferSize),
	}

	// Setup dump file and goroutine if enabled
	if ui.dumpEnabled {
		filename := fmt.Sprintf("thr%d_%s.txt", idx, time.Now().Format("15-04"))
		f, err := os.Create(filename)
		if err != nil {
			exitErr(fmt.Sprintf("Failed to create dump file %s: %v", filename, err))
		}
		ui.dumpFiles[idx] = f
		ui.dumpChans[idx] = make(chan string, logBufferSize)

		// Dump writer goroutine
		go func(f *os.File, ch <-chan string) {
			for msg := range ch {
				f.WriteString(msg)
			}
		}(f, ui.dumpChans[idx])
	}

	// Start consumer goroutine
	go ui.logConsumer(idx, tv, logger.ch)

	tabName := fmt.Sprintf("t%d", idx+1)
	ui.tabNames[idx] = tabName
	ui.loggers[idx] = logger
	ui.textViews[idx] = tv

	ui.pages.AddPage(tabName, tv, true, idx == 0)

	// Initial message
	fmt.Fprintf(tv, "Thread %d starting...\n", idx+1)
}

// logConsumer - consumes log messages and batch-updates TextView
func (ui *UIManager) logConsumer(idx int, tv *tview.TextView, ch <-chan string) {
	var batch strings.Builder
	batchSize := 0
	maxBatch := 10

	flush := func() {
		if batchSize > 0 {
			content := batch.String()
			ui.app.QueueUpdateDraw(func() {
				fmt.Fprint(tv, content)
				tv.ScrollToEnd()
			})
			// Send to dump goroutine if enabled (check closed flag)
			if ui.dumpEnabled && ui.dumpChans[idx] != nil {
				ui.dumpClosedMu.Lock()
				closed := ui.dumpClosed
				ui.dumpClosedMu.Unlock()
				if !closed {
					select {
					case ui.dumpChans[idx] <- content:
					default:
						// buffer full, skip
					}
				}
			}
			batch.Reset()
			batchSize = 0
		}
	}

	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				flush()
				return
			}
			batch.WriteString(msg)
			batchSize++

			if batchSize >= maxBatch {
				flush()
			}
		default:
			flush()
			msg, ok := <-ch
			if !ok {
				return
			}
			batch.WriteString(msg)
			batchSize++
		}
	}
}

// StartStatsConsumer - consumes stats output and updates panel
func (ui *UIManager) StartStatsConsumer(statsCh <-chan string) {
	go func() {
		for content := range statsCh {
			ui.app.QueueUpdateDraw(func() {
				ui.statsPanel.Clear()
				fmt.Fprint(ui.statsPanel, content)
			})
		}
	}()
}

// GetLogger - returns LogWriter for given thread
func (ui *UIManager) GetLogger(idx int) LogWriter {
	return ui.loggers[idx]
}

// SetupInputCapture - configures keyboard navigation
func (ui *UIManager) SetupInputCapture(orch *Orch, monkeysFinished *bool) {
	currentTab := 0

	ui.app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		if event.Key() == tcell.KeyTab {
			currentTab = (currentTab + 1) % len(ui.tabNames)
			ui.pages.SwitchToPage(ui.tabNames[currentTab])
			return nil
		} else if event.Key() == tcell.KeyBacktab {
			currentTab = (currentTab - 1 + len(ui.tabNames)) % len(ui.tabNames)
			ui.pages.SwitchToPage(ui.tabNames[currentTab])
			return nil
		} else if event.Key() == tcell.KeyEnter {
			// Resume all paused threads
			orch.resumeAll()
			return nil
		} else if event.Rune() == 'q' || event.Rune() == 'Q' {
			// Close dump files and channels
			ui.closeDumpFiles()

			if *monkeysFinished {
				ui.app.Stop()
			} else {
				select {
				case <-orch.quitChan:
				default:
					close(orch.quitChan)
				}
				ui.app.Stop()
			}
			return nil
		}
		return event
	})
}

// closeDumpFiles - closes dump channels and files
func (ui *UIManager) closeDumpFiles() {
	if !ui.dumpEnabled {
		return
	}
	// Set closed flag first to prevent sends
	ui.dumpClosedMu.Lock()
	ui.dumpClosed = true
	ui.dumpClosedMu.Unlock()

	// Small delay to let pending flushes complete
	time.Sleep(50 * time.Millisecond)

	for i, ch := range ui.dumpChans {
		if ch != nil {
			close(ch)
		}
		if ui.dumpFiles[i] != nil {
			ui.dumpFiles[i].Close()
		}
	}
}

// Run - starts tview app (blocks until exit)
func (ui *UIManager) Run() error {
	return ui.app.SetRoot(ui.mainFlex, true).EnableMouse(true).Run()
}

// BroadcastMessage - sends message to all thread loggers
func (ui *UIManager) BroadcastMessage(msg string) {
	for _, logger := range ui.loggers {
		logger.Write(msg)
	}
}

// CloseLoggers - closes all logger channels
func (ui *UIManager) CloseLoggers() {
	for _, logger := range ui.loggers {
		logger.Close()
	}
}

// PrintLogo - prints random logo to stdout
func PrintLogo() {
	fmt.Print(logo[rand.Intn(len(logo))] + "\nTransactional Racing Executor Monkey - aka Macaco " + version + "\n\n")
}
