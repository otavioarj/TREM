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

// prepareReqBase - loads request and applies regex patterns (no FIFO substitution)
func (o *Orch) prepareReqBase(w *monkey, idx int) (string, error) {
	var raw []byte
	var err error

	// Use cache if available
	if len(w.reqCache) > 0 {
		raw = []byte(w.reqCache[idx])
	} else {
		raw, err = os.ReadFile(w.reqFiles[idx])
		if err != nil {
			return "", err
		}
	}
	req := string(raw)

	// Apply pattern replacements (skip for first request)
	if idx > 0 {
		for _, p := range w.patterns[idx-1] {
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

			// Save static values (keys starting with _) for persistence across requests
			if len(p.keyword) > 0 && p.keyword[0] == '_' {
				w.staticVals["$"+p.keyword+"$"] = extracted
			}
		}
	}

	// Apply static values (extracted from previous requests with _ prefix)
	for k, v := range w.staticVals {
		if strings.Contains(req, k) {
			req = strings.ReplaceAll(req, k, v)
			w.logger.Write(fmt.Sprintf("Static %s: %s\n", k, v))
		}
	}

	return req, nil
}

// doBarrier - handles sync barrier: dial and wait for all threads
// Returns addr for subsequent sends
func (o *Orch) doBarrier(w *monkey, idx int, addr string) error {
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
func (o *Orch) processReq(w *monkey, idx int) error {
	baseReq, err := o.prepareReqBase(w, idx)
	if err != nil {
		return err
	}

	// Handle static mode
	if o.valDist.IsStatic() {
		k, v := o.valDist.StaticKV()
		req := strings.ReplaceAll(baseReq, k, v)
		addr, err := parseHost(req, o.hostFlag)
		if err != nil {
			return err
		}
		// Barrier if sync mode and this idx triggers it
		if o.mode == "sync" && o.syncBarriers[idx] {
			if err := o.doBarrier(w, idx, addr); err != nil {
				return err
			}
		}
		return o.sendReq(w, idx, req, addr)
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
		if o.mode == "sync" && o.syncBarriers[idx] {
			if err := o.doBarrier(w, idx, addr); err != nil {
				return err
			}
		}
		return o.sendReq(w, idx, baseReq, addr)
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
	// PER original requestN.txt file!
	combinations := generateCombinations(w.localBuffer, keys)
	if len(combinations) == 0 {
		// No values available, send as-is (will likely fail regex check)
		addr, err := parseHost(baseReq, o.hostFlag)
		if err != nil {
			return err
		}
		if o.mode == "sync" && o.syncBarriers[idx] {
			if err := o.doBarrier(w, idx, addr); err != nil {
				return err
			}
		}
		return o.sendReq(w, idx, baseReq, addr)
	}

	// Send request for each combination
	// Barrier triggers on FIRST combination only
	firstReq := true
	for _, combo := range combinations {
		req := baseReq
		for k, v := range combo {
			req = strings.ReplaceAll(req, k, v)
			w.logger.Write(fmt.Sprintf("Val %s: %s\n", k, v))
		}

		addr, err := parseHost(req, o.hostFlag)
		if err != nil {
			return err
		}

		// Barrier only on first request of combinations
		if firstReq && o.mode == "sync" && o.syncBarriers[idx] {
			if err := o.doBarrier(w, idx, addr); err != nil {
				return err
			}
			firstReq = false
		}

		if err := o.sendReq(w, idx, req, addr); err != nil {
			return err
		}
	}

	// Consume used values
	consumeValues(w.localBuffer, keys)
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
func (o *Orch) sendReq(w *monkey, idx int, req string, addr string) error {
	req = normalizeRequest(req)

	// Save loop start req and addr if this is the loop start point
	if o.loopStart > 0 && idx == o.loopStart-1 {
		w.loopStartReq = req
		w.loopStartAddr = addr
	}

	// Check if fire-and-forget (empty pattern line for this idx)
	fireAndForget := idx < len(w.patterns) && len(w.patterns[idx]) == 0

	var resp, status string
	var err error

	if o.keepAlive || o.httpH2 {
		// Keep-alive: reconnect if addr changed or not connected
		if addr != w.connAddr || (w.conn == nil && w.h2conn == nil) {
			o.closeWorkerConn(w)
			w.logger.Write(fmt.Sprintf("Conn-Keep (%d): %s\n", idx, addr))
			if err := o.dialWithRetry(w, addr); err != nil {
				return fmt.Errorf("dial: %v", err)
			}
		}

		// Fire-and-forget: send without waiting for response
		if fireAndForget {
			w.logger.Write(fmt.Sprintf("Fire&Forget (%d)\n", idx))
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
		w.logger.Write(fmt.Sprintf("DEBUG: depois sendWithReconnect idx=%d err=%v\n", idx, err))
	} else {
		// No keep-alive: new connection each request
		w.logger.Write(fmt.Sprintf("Conn (%d): %s\n", idx, addr))

		// Fire-and-forget in non-keepalive mode
		if fireAndForget {
			w.logger.Write(fmt.Sprintf("Fire&Forget (%d)\n", idx))
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
		w.logger.Write(fmt.Sprintf("DEBUG: depois sendWithRetry idx=%d err=%v\n", idx, err))
	}

	if err != nil {
		return err
	}

	w.logger.Write(fmt.Sprintf("HTTP (%d) %s\n", idx, status))
	w.prevResp = resp

	// Apply response actions if pattern matches
	if w.actionPatterns != nil && w.actionPatterns[idx] != nil {
		if err := o.applyResponseActions(w, idx, req, resp, status); err != nil {
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
	if !ap.re.MatchString(resp) {
		return nil
	}

	w.logger.Write(fmt.Sprintf("Action match at idx %d\n", idx+1))

	// Execute actions in order
	for _, action := range ap.actions {
		switch action.actionType {
		case ActionPrintThread:
			o.pauseThread(w, action.arg)

		case ActionPrintAll:
			o.pauseAllThreads(action.arg)

		case ActionSaveReq:
			if err := saveToFile(action.arg, idx, req); err != nil {
				w.logger.Write(fmt.Sprintf("Save req error: %v\n", err))
				return err
			}
			w.logger.Write(fmt.Sprintf("Saved request to %s\n", action.arg))
			o.pauseThread(w, "Request saved")

		case ActionSaveResp:
			respToSave := resp
			if o.httpH2 {
				respToSave = formatH2ResponseAsH1(resp, status)
			}
			if err := saveToFile(action.arg, idx, respToSave); err != nil {
				w.logger.Write(fmt.Sprintf("Save resp error: %v\n", err))
				return err
			}
			w.logger.Write(fmt.Sprintf("Saved response to %s\n", action.arg))
			o.pauseThread(w, "Response saved")

		case ActionSaveAll:
			respToSave := resp
			if o.httpH2 {
				respToSave = formatH2ResponseAsH1(resp, status)
			}
			content := req + " " + respToSave
			if err := saveToFile(action.arg, idx, content); err != nil {
				w.logger.Write(fmt.Sprintf("Save all error: %v\n", err))
				return err
			}
			w.logger.Write(fmt.Sprintf("Saved req+resp to %s\n", action.arg))
			o.pauseThread(w, "Request and response saved")

		case ActionExit:
			w.logger.Write("Exit action triggered\n")
			// Graceful exit - close quitChan
			select {
			case <-o.quitChan:
				// Already closed
			default:
				close(o.quitChan)
			}
			return fmt.Errorf("exit action")
		}
	}

	return nil
}
