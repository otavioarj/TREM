package main

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// ParsedReq - protocol-agnostic request structure
type ParsedReq struct {
	Method  string
	Path    string
	Host    string
	Headers [][2]string // preserves order
	Body    []byte
}

// parseRawReq2H2 - parses HTTP/1.1 raw text into HTTP2 ParsedReq
//
//	TREM always get request as HTTP/1.1, so pseudo-header
//	ARE IGNORED here!
func parseRawReq2H2(raw string) *ParsedReq {
	lines := strings.Split(raw, "\r\n")
	if len(lines) == 0 {
		return nil
	}

	req := &ParsedReq{
		Headers: make([][2]string, 0, 16),
	}

	// Parse request line: METHOD PATH HTTP/1.1
	parts := strings.SplitN(lines[0], " ", 3)
	if len(parts) >= 2 {
		req.Method = parts[0]
		req.Path = parts[1]
	}

	// Parse headers
	bodyIdx := -1
	for i := 1; i < len(lines); i++ {
		line := lines[i]
		if line == "" {
			bodyIdx = i + 1
			break
		}

		idx := strings.Index(line, ":")
		if idx > 0 {
			key := strings.TrimSpace(line[:idx])
			val := strings.TrimSpace(line[idx+1:])

			// Extract host for :authority pseudo-header
			if strings.EqualFold(key, "Host") {
				req.Host = val
			}

			// SKIP headers that become pseudo-headers in h2
			// or are connection-specific (not allowed in h2)
			lower := strings.ToLower(key)
			if lower == "host" || lower == "connection" ||
				lower == "keep-alive" || lower == "transfer-encoding" ||
				lower == "upgrade" || lower == "proxy-connection" {
				continue
			}

			req.Headers = append(req.Headers, [2]string{key, val})
		}
	}

	// Extract body
	if bodyIdx > 0 && bodyIdx < len(lines) {
		req.Body = []byte(strings.Join(lines[bodyIdx:], "\r\n"))
	}

	return req
}

// prepareReq - loads request (absIdx) and applies patterns (relIdx)
// Single mode: relIdx == absIdx
// Group mode: relIdx = position in group, absIdx = position in reqFiles
func (o *Orch) prepareReq(w *monkey, relIdx, absIdx int) (string, error) {
	var raw []byte
	var err error

	// Use cache if available (indexed by absolute index)
	if len(w.reqCache) > 0 {
		raw = []byte(w.reqCache[absIdx])
	} else {
		raw, err = os.ReadFile(w.reqFiles[absIdx])
		if err != nil {
			return "", err
		}
	}
	req := string(raw)

	// Apply pattern replacements (skip for first request in chain)
	if relIdx > 0 && len(w.patterns) > relIdx-1 {
		for _, p := range w.patterns[relIdx-1] {
			// Skip extraction if static value already exists in global store
			if len(p.keyword) > 0 && p.keyword[0] == '_' {
				if _, exists := globalStaticVals.Load(p.keyword); exists {
					continue
				}
			}

			m := p.re.FindStringSubmatch(w.prevResp)
			if m == nil {
				if verbose {
					w.logger.Write(fmt.Sprintf("[V] regex no match, pattern: %s\n", p.keyword))
				}
				return "", fmt.Errorf("regex did not match for: %s", p.keyword)
			}
			extracted := decodeExtracted(m[1])
			req = strings.ReplaceAll(req, "$"+p.keyword+"$", extracted)
			w.logger.Write(fmt.Sprintf("Matched %s: %s\n", p.keyword, extracted))

			// Save static values (keys starting with _) to global store for cross-group access
			if len(p.keyword) > 0 && p.keyword[0] == '_' {
				globalStaticVals.Store(p.keyword, extracted)
			}
		}
	}

	// Apply static values from global store (extracted from previous requests with _ prefix)
	globalStaticVals.Range(func(key, value interface{}) bool {
		k := key.(string)
		v := value.(string)
		fullKey := "$" + k + "$"
		if strings.Contains(req, fullKey) {
			req = strings.ReplaceAll(req, fullKey, v)
			w.logger.Write(fmt.Sprintf("Static %s: %s\n", fullKey, v))
		}
		return true
	})

	return req, nil
}

// doBarrier - handles sync barrier: dial and wait for all threads
// Returns addr for subsequent sends
func (o *Orch) doBarrier(w *monkey, relIdx int, addr string) error {
	// Dial if not connected or addr changed
	if w.conn == nil && w.h2conn == nil {
		w.logger.Write(fmt.Sprintf("Connecting: %s\n", addr))
		if err := o.dialWithRetry(w, addr); err != nil {
			return fmt.Errorf("dial: %v", err)
		}
	} else if addr != w.connAddr {
		// Address changed, reconnect
		o.closeWorkerConn(w)
		w.logger.Write(fmt.Sprintf("Reconnecting: %s\n", addr))
		if err := o.dialWithRetry(w, addr); err != nil {
			return fmt.Errorf("dial: %v", err)
		}
	}

	// Signal ready and wait for barrier release
	o.readyChan <- w.id
	w.logger.Write("Ready, waiting sync...\n")
	<-o.startChan
	w.logger.Write("Sync!\n")

	return nil
}

// processReq - unified request processor for sync/async modes with FIFO combinations
// relIdx = relative index within group (for patterns and barriers)
// absIdx = absolute index in reqFiles (for loading request and response actions)
func (o *Orch) processReq(w *monkey, relIdx, absIdx int) error {
	baseReq, err := o.prepareReq(w, relIdx, absIdx)
	if err != nil {
		return err
	}

	// Handle static mode
	if o.valDist.IsStatic() {
		k, v := o.valDist.StaticKV()
		req := strings.ReplaceAll(baseReq, "$"+k+"$", v)
		addr, err := parseHost(req, o.hostFlag)
		if err != nil {
			return err
		}
		// Barrier uses relIdx (relative to group)
		if o.mode == "sync" && o.syncBarriers[relIdx] {
			if err := o.doBarrier(w, relIdx, addr); err != nil {
				return err
			}
		}
		return o.sendReq(w, relIdx, absIdx, req, addr)
	}

	// Handle FIFO mode - drain channel first (limited by fifoBlockSize)
	drainChannel(w, o.fifoBlockSize)

	// Extract keys needed from request
	keys := extractKeys(baseReq)
	if len(keys) == 0 {
		// No FIFO keys, just send
		addr, err := parseHost(baseReq, o.hostFlag)
		if err != nil {
			return err
		}
		if o.mode == "sync" && o.syncBarriers[relIdx] {
			if err := o.doBarrier(w, relIdx, addr); err != nil {
				return err
			}
		}
		return o.sendReq(w, relIdx, absIdx, baseReq, addr)
	}

	// Check for missing keys
	missing := checkMissingKeys(w.localBuffer, keys)
	if len(missing) > 0 {
		if o.fifoWait {
			// Block waiting, waitForKeys will emit periodic messages
			if !waitForKeys(w, keys, o.quitChan) {
				w.logger.Write("FIFO closed while waiting for keys\n")
				return fmt.Errorf("FIFO timeout waiting for keys: %v", missing)
			}
		} else {
			// Warn and continue without substitution
			for _, k := range missing {
				w.logger.Write(fmt.Sprintf("Key %s not found in FIFO, sending anyway!\n", k))
			}
		}
	}

	// Generate combinations: each key=value creates a new request
	combinations := generateCombinations(w.localBuffer, keys)
	if len(combinations) == 0 {
		// No values available, send as-is
		addr, err := parseHost(baseReq, o.hostFlag)
		if err != nil {
			return err
		}
		if o.mode == "sync" && o.syncBarriers[relIdx] {
			if err := o.doBarrier(w, relIdx, addr); err != nil {
				return err
			}
		}
		return o.sendReq(w, relIdx, absIdx, baseReq, addr)
	}

	// Send request for each combination
	// Barrier triggers on FIRST combination only
	firstReq := true
	for _, combo := range combinations {
		req := baseReq
		for k, v := range combo {
			req = strings.ReplaceAll(req, "$"+k+"$", v)
			w.logger.Write(fmt.Sprintf("Val %s: %s\n", k, v))
		}

		addr, err := parseHost(req, o.hostFlag)
		if err != nil {
			return err
		}

		// Barrier only on first request of combinations
		if firstReq && o.mode == "sync" && o.syncBarriers[relIdx] {
			if err := o.doBarrier(w, relIdx, addr); err != nil {
				return err
			}
			firstReq = false
		}

		if err := o.sendReq(w, relIdx, absIdx, req, addr); err != nil {
			return err
		}
	}

	// Consume used values
	consumeValues(w.localBuffer, keys, o.fifoBlockSize)
	return nil
}

func normalizeRequest(req string) string {
	lines := strings.Split(req, "\n")
	var normalized []string
	var bodyStartIndex int = -1
	var hasEmptyLine bool

	for i, line := range lines {
		line = strings.TrimRight(line, "\r")
		if i == 0 && !strings.Contains(line, "HTTP/") {
			line = line + " HTTP/1.1"
		} else if i == 0 && strings.Contains(line, "HTTP/") && !strings.Contains(line, "HTTP/1.1") {
			line = httpVersionRe.ReplaceAllString(line, "HTTP/1.1")
		}

		normalized = append(normalized, line)

		if line == "" && bodyStartIndex == -1 {
			bodyStartIndex = i + 1
			hasEmptyLine = true
		}
	}

	// Ensure empty line exists after headers (before body or at end)
	if !hasEmptyLine {
		normalized = append(normalized, "")
		bodyStartIndex = len(normalized)
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

// sendReq - unified request sender for both sync/async modes
// relIdx = relative index (for patterns/fire-and-forget/loop detection)
// absIdx = absolute index (for logging and response actions)
func (o *Orch) sendReq(w *monkey, relIdx, absIdx int, req string, addr string) error {
	req = normalizeRequest(req)

	// Save loop start req and addr (using relIdx for comparison)
	if o.loopStart > 0 && relIdx == o.loopStart-1 {
		w.loopStartReq = req
		w.loopStartAddr = addr
	}

	// Fire-and-forget detection uses relIdx (group patterns)
	fireAndForget := relIdx < len(w.patterns) && len(w.patterns[relIdx]) == 0

	var resp, status string
	var err error

	if o.keepAlive || o.httpH2 {
		// Keep-alive: reconnect if addr changed or not connected
		if addr != w.connAddr || (w.conn == nil && w.h2conn == nil) {
			o.closeWorkerConn(w)
			w.logger.Write(fmt.Sprintf("Conn-Keep (r%d): %s\n", absIdx+1, addr))
			if err := o.dialWithRetry(w, addr); err != nil {
				return fmt.Errorf("dial: %v", err)
			}
		}

		// Fire-and-forget: send without waiting for response
		if fireAndForget {
			w.logger.Write(fmt.Sprintf("Fire&Forget (r%d)\n", absIdx+1))
			bytesOut := uint32(len(req))
			if o.httpH2 {
				// HTTP/2: send headers+data frames only
				parsedReq := parseRawReq2H2(req)
				if parsedReq == nil {
					return fmt.Errorf("failed to parse request for fire-and-forget")
				}
				if _, _, err := w.h2conn.sendReqH2(parsedReq, 0, false); err != nil {
					return err
				}
			} else {
				// HTTP/1.1: write and close
				_, err = w.conn.Write([]byte(req))
				if err != nil {
					return err
				}
			}
			// Report stats for fire-and-forget (no latency, no bytes in)
			stats.ReportRequest(w.id, 0, 0, bytesOut)
			o.closeWorkerConn(w) // force new connection for next request
			w.prevResp = ""
			return nil
		}

		resp, status, err = o.sendWithReconnect(w, []byte(req), addr)
	} else {
		// No keep-alive: new connection each request
		w.logger.Write(fmt.Sprintf("Conn (r%d): %s\n", absIdx+1, addr))

		// Fire-and-forget in non-keepalive mode
		if fireAndForget {
			w.logger.Write(fmt.Sprintf("Fire&Forget (r%d)\n", absIdx+1))
			conn, dialErr := o.dialNewConn(addr)
			if dialErr != nil {
				return dialErr
			}
			_, err = conn.Write([]byte(req))
			conn.Close()
			if err != nil {
				return err
			}
			// Report stats for fire-and-forget (no latency, no bytes in)
			stats.ReportRequest(w.id, 0, 0, uint32(len(req)))
			w.prevResp = ""
			return nil
		}

		resp, status, err = o.sendWithRetry(w, []byte(req), addr)
	}

	if err != nil {
		return err
	}

	w.logger.Write(fmt.Sprintf("HTTP (r%d) %s\n", absIdx+1, status))
	w.prevResp = resp

	// Apply response actions (uses absIdx for global pattern matching)
	if w.actionPatterns != nil && w.actionPatterns[absIdx] != nil {
		if err := o.applyResponseActions(w, absIdx, req, resp, status); err != nil {
			return err
		}
	}

	// Close if not keepalive (HTTP/1.1 only, H2 always reuses)
	if !o.keepAlive && !o.httpH2 {
		o.closeWorkerConn(w)
	}

	return nil
}

func loadPatterns(path string) ([][]pattern, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")

	// Filter comment lines (keep empty for fire-and-forget)
	var filtered []string
	for _, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		filtered = append(filtered, line)
	}
	lines = filtered

	patterns := make([][]pattern, len(lines))
	for i, line := range lines {
		line = strings.TrimSpace(line)

		// Empty line = fire-and-forget marker, keep empty slice for indexing
		if line == "" {
			patterns[i] = []pattern{}
			continue
		}

		// multiple patterns per line separated by <space>$<space>!
		parts := strings.Split(line, " $ ")
		patterns[i] = make([]pattern, len(parts))
		for j, part := range parts {
			bef, aft, found := strings.Cut(part, "`:")
			if !found {
				return nil, errors.New("invalid regex line: " + line)
			}
			r, err := regexp.Compile(bef)
			if err != nil {
				return nil, err
			}
			patterns[i][j] = pattern{re: r, keyword: aft}
		}
	}
	return patterns, nil
}

// applyResponseActions - applies response actions if pattern matches
// Returns error on exit action or save failure
func (o *Orch) applyResponseActions(w *monkey, idx int, req, resp, status string) error {
	ap := w.actionPatterns[idx]
	if ap == nil {
		return nil
	}

	// Check if regex matches response
	m := ap.re.FindStringSubmatch(resp)
	if m == nil {
		return nil // No match, continue normally
	}

	w.logger.Write(fmt.Sprintf("RA matched (r%d): %s\n", idx+1, ap.re.String()))

	// Execute actions in order
	for _, action := range ap.actions {
		switch action.actionType {
		case ActionPrintAll:
			// Broadcast message to all threads and pause all
			msg := fmt.Sprintf("\n>>> RA (r%d) T%d: %s <<<\n", idx+1, w.id+1, action.arg)
			o.uiManager.BroadcastMessage(msg)
			o.pauseAllThreads()
			o.checkPause(w)

		case ActionPrintThread:
			// Print to this thread only and pause it
			w.logger.Write(fmt.Sprintf(">>> RA: %s <<<\n", action.arg))
			o.pauseThread(w.id)
			o.checkPause(w)

		case ActionSaveReq:
			// Save request that generated match
			if err := os.WriteFile(action.arg, []byte(req), 0644); err != nil {
				w.logger.Write(fmt.Sprintf("RA save error: %v\n", err))
			} else {
				w.logger.Write(fmt.Sprintf("RA saved req to: %s\n", action.arg))
			}
			o.pauseThread(w.id)
			o.checkPause(w)

		case ActionSaveResp:
			// Save response that generated match
			if err := os.WriteFile(action.arg, []byte(resp), 0644); err != nil {
				w.logger.Write(fmt.Sprintf("RA save error: %v\n", err))
			} else {
				w.logger.Write(fmt.Sprintf("RA saved resp to: %s\n", action.arg))
			}
			o.pauseThread(w.id)
			o.checkPause(w)

		case ActionSaveAll:
			// Save both request and response
			content := fmt.Sprintf("=== REQUEST ===\n%s\n\n=== RESPONSE ===\n%s", req, resp)
			if err := os.WriteFile(action.arg, []byte(content), 0644); err != nil {
				w.logger.Write(fmt.Sprintf("RA save error: %v\n", err))
			} else {
				w.logger.Write(fmt.Sprintf("RA saved req+resp to: %s\n", action.arg))
			}
			o.pauseThread(w.id)
			o.checkPause(w)

		case ActionExit:
			// Graceful exit - close quitChan to signal all workers
			w.logger.Write("RA: exit triggered\n")
			select {
			case <-o.quitChan:
				// Already closed
			default:
				close(o.quitChan)
			}
			return fmt.Errorf("RA exit")
		}
	}

	return nil
}
