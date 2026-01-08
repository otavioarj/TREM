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

// GroupInfo - UI representation of a thread group
type GroupInfo struct {
	ID          int
	Name        string
	ThreadCount int
	Mode        string
	ReqIndices  []int
}

// UIManager - manages tview app, thread tabs, and stats panel
type UIManager struct {
	app        *tview.Application
	mainFlex   *tview.Flex
	treeView   *tview.TreeView
	contentBox *tview.TextView
	statsPanel *tview.TextView

	// Group structure
	groups    []GroupInfo
	hasGroups bool // true if using -thrG mode

	// Thread data (indexed by global threadID)
	loggers     []*AsyncLogWriter
	textViews   []*tview.TextView
	threadNodes []*tview.TreeNode // for navigation

	// Dump support
	dumpEnabled  bool
	dumpFiles    []*os.File
	dumpChans    []chan string
	dumpClosed   bool
	dumpClosedMu sync.Mutex
}

// NewUIManager - creates UI with optional group support
func NewUIManager(totalThreads int, dumpEnabled bool) *UIManager {
	ui := &UIManager{
		app:         tview.NewApplication(),
		loggers:     make([]*AsyncLogWriter, totalThreads),
		textViews:   make([]*tview.TextView, totalThreads),
		threadNodes: make([]*tview.TreeNode, totalThreads),
		dumpEnabled: dumpEnabled,
		hasGroups:   false,
	}

	if dumpEnabled {
		ui.dumpFiles = make([]*os.File, totalThreads)
		ui.dumpChans = make([]chan string, totalThreads)
	}

	return ui
}

// SetupGroups - configures UI for group mode
func (ui *UIManager) SetupGroups(groups []*ThreadGroup) {
	ui.hasGroups = true
	ui.groups = make([]GroupInfo, len(groups))

	for i, g := range groups {
		ui.groups[i] = GroupInfo{
			ID:          g.ID,
			Name:        fmt.Sprintf("G%d", g.ID+1),
			ThreadCount: g.ThreadCount,
			Mode:        g.Mode,
			ReqIndices:  g.ReqIndices,
		}
	}
}

// SetupSingleMode - configures UI for single group (original behavior)
func (ui *UIManager) SetupSingleMode(threadCount int, reqCount int) {
	ui.hasGroups = false
	indices := make([]int, reqCount)
	for i := 0; i < reqCount; i++ {
		indices[i] = i
	}
	ui.groups = []GroupInfo{{
		ID:          0,
		Name:        "Main",
		ThreadCount: threadCount,
		Mode:        "async",
		ReqIndices:  indices,
	}}
}

// Build - constructs the UI layout (call after SetupGroups/SetupSingleMode)
func (ui *UIManager) Build() {
	// Create all thread views and loggers
	globalID := 0
	for _, group := range ui.groups {
		for t := 0; t < group.ThreadCount; t++ {
			ui.createThreadView(globalID, group.ID, t)
			globalID++
		}
	}

	// Build tree view (returns first node for later selection)
	firstNode := ui.buildTreeView()

	// Stats panel
	ui.statsPanel = tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(false)
	ui.statsPanel.SetBorder(true).SetTitle(" Stats ")

	// Content area (shows selected thread)
	ui.contentBox = ui.textViews[0]

	// Layout
	if ui.hasGroups {
		// TreeView layout for groups
		leftPanel := tview.NewFlex().
			SetDirection(tview.FlexRow).
			AddItem(ui.treeView, 0, 1, true)

		rightPanel := tview.NewFlex().
			SetDirection(tview.FlexRow).
			AddItem(ui.contentBox, 0, 2, false).
			AddItem(ui.statsPanel, 0, 1, false)

		ui.mainFlex = tview.NewFlex().
			SetDirection(tview.FlexColumn).
			AddItem(leftPanel, 20, 0, true).
			AddItem(rightPanel, 0, 1, false)
	} else {
		// Simple layout for single mode
		ui.mainFlex = tview.NewFlex().
			SetDirection(tview.FlexRow).
			AddItem(ui.contentBox, 0, 2, true).
			AddItem(ui.statsPanel, 0, 1, false)
	}

	// Set initial tree selection AFTER mainFlex exists (callbacks may fire)
	if firstNode != nil && ui.hasGroups {
		ui.treeView.SetCurrentNode(firstNode)
	}
}

// createThreadView - creates TextView and logger for a thread
func (ui *UIManager) createThreadView(globalID, groupID, localID int) {
	tv := tview.NewTextView().
		SetDynamicColors(true).
		SetScrollable(true)

	var title string
	if ui.hasGroups {
		title = fmt.Sprintf(" G%d:T%d | Tab:Next Thread Ctlr+P:Next Group Q:Quit", groupID+1, localID+1)
	} else {
		title = fmt.Sprintf(" Thread %d | Tab:Next Shift+Tab:Prev Q:Quit", globalID+1)
	}
	tv.SetBorder(true).SetTitle(title)

	// Async logger
	logger := &AsyncLogWriter{
		ch: make(chan string, logBufferSize),
	}

	// Setup dump file if enabled
	if ui.dumpEnabled {
		var filename string
		if ui.hasGroups {
			filename = fmt.Sprintf("g%d_t%d_%s.txt", groupID+1, localID+1, time.Now().Format("15-04"))
		} else {
			filename = fmt.Sprintf("thr%d_%s.txt", globalID, time.Now().Format("15-04"))
		}
		f, err := os.Create(filename)
		if err != nil {
			exitErr(fmt.Sprintf("Failed to create dump file %s: %v", filename, err))
		}
		ui.dumpFiles[globalID] = f
		ui.dumpChans[globalID] = make(chan string, logBufferSize)

		go func(f *os.File, ch <-chan string) {
			for msg := range ch {
				f.WriteString(msg)
			}
		}(f, ui.dumpChans[globalID])
	}

	// Start consumer goroutine
	go ui.logConsumer(globalID, tv, logger.ch)

	ui.loggers[globalID] = logger
	ui.textViews[globalID] = tv

	// Initial message
	if ui.hasGroups {
		fmt.Fprintf(tv, "Group %d, Thread %d starting...\n", groupID+1, localID+1)
	} else {
		fmt.Fprintf(tv, "Thread %d starting...\n", globalID+1)
	}
}

// buildTreeView - creates hierarchical tree view for groups/threads
// Returns the first thread node for initial selection (must be set after mainFlex exists)
func (ui *UIManager) buildTreeView() *tview.TreeNode {
	root := tview.NewTreeNode("TREM").SetColor(tcell.ColorYellow)
	ui.treeView = tview.NewTreeView().
		SetRoot(root).
		SetCurrentNode(root)
	ui.treeView.SetBorder(true).SetTitle("Groups")

	globalID := 0
	var firstNode *tview.TreeNode

	for _, group := range ui.groups {
		// Group node
		groupLabel := fmt.Sprintf("G%d [%s]", group.ID+1, group.Mode)
		groupNode := tview.NewTreeNode(groupLabel).
			SetColor(tcell.ColorGreen).
			SetExpanded(true).
			SetSelectable(true)

		// Thread nodes under group
		for t := 0; t < group.ThreadCount; t++ {
			threadLabel := fmt.Sprintf("T%d", t+1)
			threadNode := tview.NewTreeNode(threadLabel).
				SetColor(tcell.ColorWhite).
				SetReference(globalID). // store global thread ID
				SetSelectable(true)

			ui.threadNodes[globalID] = threadNode
			groupNode.AddChild(threadNode)

			if firstNode == nil {
				firstNode = threadNode
			}
			globalID++
		}

		root.AddChild(groupNode)
	}

	// Handle selection changes
	ui.treeView.SetSelectedFunc(func(node *tview.TreeNode) {
		ref := node.GetReference()
		if ref != nil {
			if gid, ok := ref.(int); ok {
				ui.updateContentView(gid)
			}
		}
	})

	ui.treeView.SetChangedFunc(func(node *tview.TreeNode) {
		ref := node.GetReference()
		if ref != nil {
			if gid, ok := ref.(int); ok {
				ui.updateContentView(gid)
			}
		}
	})

	return firstNode
}

// updateContentView - switches the content panel to show selected thread
func (ui *UIManager) updateContentView(globalThreadID int) {
	if globalThreadID < 0 || globalThreadID >= len(ui.textViews) {
		return
	}

	newContent := ui.textViews[globalThreadID]

	// Update the right panel content
	if ui.hasGroups {
		// Find the right panel (second item in mainFlex)
		rightPanel := ui.mainFlex.GetItem(1).(*tview.Flex)
		rightPanel.Clear()
		rightPanel.AddItem(newContent, 0, 2, false)
		rightPanel.AddItem(ui.statsPanel, 0, 1, false)
	} else {
		ui.mainFlex.Clear()
		ui.mainFlex.AddItem(newContent, 0, 2, true)
		ui.mainFlex.AddItem(ui.statsPanel, 0, 1, false)
	}

	ui.contentBox = newContent
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
			// Send to dump goroutine if enabled
			if ui.dumpEnabled && ui.dumpChans[idx] != nil {
				ui.dumpClosedMu.Lock()
				closed := ui.dumpClosed
				ui.dumpClosedMu.Unlock()
				if !closed {
					select {
					case ui.dumpChans[idx] <- content:
					default:
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

// GetLogger - returns LogWriter for given global thread ID
func (ui *UIManager) GetLogger(globalThreadID int) LogWriter {
	return ui.loggers[globalThreadID]
}

// SetupInputCapture - configures keyboard navigation
func (ui *UIManager) SetupInputCapture(orchs []*Orch, monkeysFinished *bool) {
	currentIdx := 0
	totalThreads := len(ui.textViews)

	ui.app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
		switch event.Key() {
		case tcell.KeyTab:
			// Next thread
			currentIdx = (currentIdx + 1) % totalThreads
			ui.selectThread(currentIdx)
			return nil

		case tcell.KeyBacktab:
			// Previous thread
			currentIdx = (currentIdx - 1 + totalThreads) % totalThreads
			ui.selectThread(currentIdx)
			return nil

		case tcell.KeyCtrlP:
			// Previous group
			currentIdx = ui.prevGroupFirstThread(currentIdx)
			ui.selectThread(currentIdx)
			return nil

		case tcell.KeyEnter:
			// Resume all paused threads
			for _, o := range orchs {
				o.resumeAll()
			}
			return nil

		case tcell.KeyRune:
			switch event.Rune() {
			case 'q', 'Q':
				ui.closeDumpFiles()
				if *monkeysFinished {
					ui.app.Stop()
				} else {
					for _, o := range orchs {
						select {
						case <-o.quitChan:
						default:
							close(o.quitChan)
						}
					}
					ui.app.Stop()
				}
				return nil
			}
		}
		return event
	})
}

// selectThread - updates tree selection and content view
func (ui *UIManager) selectThread(globalIdx int) {
	if globalIdx < 0 || globalIdx >= len(ui.threadNodes) {
		return
	}
	node := ui.threadNodes[globalIdx]
	if node != nil {
		ui.treeView.SetCurrentNode(node)
		ui.updateContentView(globalIdx)
	}
}

// nextGroupFirstThread - returns first thread of next group
func (ui *UIManager) nextGroupFirstThread(currentIdx int) int {
	offset := 0
	for i, g := range ui.groups {
		if currentIdx < offset+g.ThreadCount {
			// Current is in group i, go to group i+1
			nextGroup := (i + 1) % len(ui.groups)
			nextOffset := 0
			for j := 0; j < nextGroup; j++ {
				nextOffset += ui.groups[j].ThreadCount
			}
			return nextOffset
		}
		offset += g.ThreadCount
	}
	return 0
}

// prevGroupFirstThread - returns first thread of previous group
func (ui *UIManager) prevGroupFirstThread(currentIdx int) int {
	offset := 0
	for i, g := range ui.groups {
		if currentIdx < offset+g.ThreadCount {
			// Current is in group i, go to group i-1
			prevGroup := (i - 1 + len(ui.groups)) % len(ui.groups)
			prevOffset := 0
			for j := 0; j < prevGroup; j++ {
				prevOffset += ui.groups[j].ThreadCount
			}
			return prevOffset
		}
		offset += g.ThreadCount
	}
	return 0
}

// closeDumpFiles - closes dump channels and files
func (ui *UIManager) closeDumpFiles() {
	if !ui.dumpEnabled {
		return
	}
	ui.dumpClosedMu.Lock()
	ui.dumpClosed = true
	ui.dumpClosedMu.Unlock()

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

// BroadcastToGroup - sends message to all threads in a group
func (ui *UIManager) BroadcastToGroup(groupID int, msg string) {
	offset := 0
	for _, g := range ui.groups {
		if g.ID == groupID {
			for i := 0; i < g.ThreadCount; i++ {
				ui.loggers[offset+i].Write(msg)
			}
			return
		}
		offset += g.ThreadCount
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
