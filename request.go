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

// prepareReq - loads request template and applies pattern extractions
// relIdx: relative index in group (for patterns)
// absIdx: absolute index in reqFiles (for file loading)
// Returns: template, values from patterns+static, error
func (o *Orch) prepareReq(w *monkey, relIdx, absIdx int) (*TemplateReq, map[string]string, error) {
	var raw string

	// use cache if available
	if len(w.reqCache) > 0 {
		raw = w.reqCache[absIdx]
	} else {
		data, err := os.ReadFile(w.reqFiles[absIdx])
		if err != nil {
			return nil, nil, err
		}
		raw = string(data)
	}

	// get parsed template from cache
	tmpl := getTemplate(w.reqFiles[absIdx], raw)
	values := make(map[string]string)

	// Phase 1: Apply pattern replacements (skip for first request in chain)
	// Original behavior: patterns[relIdx-1] contains patterns for transition TO request relIdx
	if relIdx > 0 && len(w.patterns) > relIdx-1 {
		patternVals, err := applyPatterns(w.patterns[relIdx-1], w.prevResp, w.logger)
		if err != nil {
			return nil, nil, err
		}
		for k, v := range patternVals {
			values[k] = v
		}
	}

	// Phase 2: Apply static values from global store (extracted from previous requests with _ prefix)
	keyNames := getUniqueKeyNames(tmpl)
	staticVals := collectStaticValues(keyNames, w.logger)
	for k, v := range staticVals {
		if _, exists := values[k]; !exists {
			values[k] = v
		}
	}

	return tmpl, values, nil
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

// processReq - unified request processor for sync/async modes
// relIdx = relative index within group (for patterns and barriers)
// absIdx = absolute index in reqFiles (for loading request and response actions)
func (o *Orch) processReq(w *monkey, relIdx, absIdx int) error {
	// Phase 1: load template and apply patterns
	tmpl, values, err := o.prepareReq(w, relIdx, absIdx)
	if err != nil {
		return err
	}

	// Phase 2: handle static mode (-u key=val) - direct send
	if o.valDist != nil && o.valDist.IsStatic() {
		k, v := o.valDist.StaticKV()
		if k != "" {
			values[k] = v
		}
		req := buildRequest(tmpl, values)
		req = normalizeReq(req)
		addr, err := parseHost(req, o.hostFlag)
		if err != nil {
			return err
		}
		if o.mode == "sync" && o.syncBarriers[relIdx] {
			if err := o.doBarrier(w, relIdx, addr); err != nil {
				return err
			}
		}
		return o.sendReq(w, relIdx, absIdx, req, addr)
	}

	// Phase 3: handle FIFO mode - drain channel first
	if o.valDist != nil && o.valDist.IsFifo() {
		drainChannel(w, o.fifoBlockSize)
	}

	// Phase 4: get keys still needing values
	keyNames := getUniqueKeyNames(tmpl)
	var keys []string
	for _, key := range keyNames {
		if _, hasVal := values[key]; !hasVal {
			keys = append(keys, key)
		}
	}

	// No additional keys needed, send directly
	if len(keys) == 0 {
		req := buildRequest(tmpl, values)
		req = normalizeReq(req)
		addr, err := parseHost(req, o.hostFlag)
		if err != nil {
			return err
		}
		if o.mode == "sync" && o.syncBarriers[relIdx] {
			if err := o.doBarrier(w, relIdx, addr); err != nil {
				return err
			}
		}
		return o.sendReq(w, relIdx, absIdx, req, addr)
	}

	// Phase 5: check for missing keys in FIFO buffer
	missing := checkMissingKeys(w.localBuffer, keys)
	if len(missing) > 0 {
		// check globalStaticVals for _key patterns
		n := 0
		for _, k := range missing {
			if len(k) > 0 && k[0] == '_' {
				if v, exists := globalStaticVals.Load(k); exists {
					values[k] = v.(string)
					w.logger.Write(fmt.Sprintf("Static $%s$: %s\n", k, v.(string)))
					continue
				}
			}
			missing[n] = k
			n++
		}
		missing = missing[:n]

		if len(missing) > 0 && o.fifoWait {
			if !waitForKeys(w, keys, o.quitChan) {
				w.logger.Write("FIFO closed while waiting for keys\n")
				return fmt.Errorf("FIFO timeout waiting for keys: %v", missing)
			}
		} else if len(missing) > 0 {
			for _, k := range missing {
				w.logger.Write(fmt.Sprintf("Key %s not found in FIFO, sending anyway!\n", k))
			}
		}
	}

	// Phase 6: generate combinations from FIFO buffer
	combinations := generateCombinations(w.localBuffer, keys)
	if len(combinations) == 0 {
		// no values available, send as-is
		req := buildRequest(tmpl, values)
		req = normalizeReq(req)
		addr, err := parseHost(req, o.hostFlag)
		if err != nil {
			return err
		}
		if o.mode == "sync" && o.syncBarriers[relIdx] {
			if err := o.doBarrier(w, relIdx, addr); err != nil {
				return err
			}
		}
		return o.sendReq(w, relIdx, absIdx, req, addr)
	}

	// Phase 7: send request for each combination
	firstReq := true
	for _, combo := range combinations {
		// merge pattern/static values with combo values
		mergedVals := make(map[string]string, len(values)+len(combo))
		for k, v := range values {
			mergedVals[k] = v
		}
		for k, v := range combo {
			mergedVals[k] = v
			w.logger.Write(fmt.Sprintf("Val %s: %s\n", k, v))
		}

		req := buildRequest(tmpl, mergedVals)
		req = normalizeReq(req)
		addr, err := parseHost(req, o.hostFlag)
		if err != nil {
			return err
		}

		// barrier only on first request of combinations
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

	// consume used values
	consumeValues(w.localBuffer, keys, o.fifoBlockSize)
	return nil
}

// sendReq - sends prepared request (already normalized)
// relIdx = relative index (for patterns/fire-and-forget/loop detection)
// absIdx = absolute index (for logging and response actions)
func (o *Orch) sendReq(w *monkey, relIdx, absIdx int, req string, addr string) error {
	// save loop start req and addr
	if o.loopStart > 0 && relIdx == o.loopStart-1 {
		w.loopStartReq = req
		w.loopStartAddr = addr
	}

	// fire-and-forget detection uses relIdx (group patterns)
	fireAndForget := relIdx < len(w.patterns) && len(w.patterns[relIdx]) == 0

	var resp, status string
	var err error

	if o.keepAlive || o.httpH2 {
		// keep-alive: reconnect if addr changed or not connected
		if addr != w.connAddr || (w.conn == nil && w.h2conn == nil) {
			o.closeWorkerConn(w)
			w.logger.Write(fmt.Sprintf("Conn-Keep (r%d): %s\n", absIdx+1, addr))
			if err := o.dialWithRetry(w, addr); err != nil {
				return fmt.Errorf("dial: %v", err)
			}
		}

		// fire-and-forget: send without waiting for response
		if fireAndForget {
			w.logger.Write(fmt.Sprintf("Fire&Forget (r%d)\n", absIdx+1))
			bytesOut := uint32(len(req))
			if o.httpH2 {
				parsedReq := parseRawReq2H2(req)
				if parsedReq == nil {
					return fmt.Errorf("failed to parse request for fire-and-forget")
				}
				if _, _, err := w.h2conn.sendReqH2(parsedReq, 0, false); err != nil {
					return err
				}
			} else {
				_, err = w.conn.Write([]byte(req))
				if err != nil {
					return err
				}
			}
			stats.ReportRequest(w.id, 0, 0, bytesOut)
			o.closeWorkerConn(w)
			w.prevResp = ""
			return nil
		}

		resp, status, err = o.sendWithReconnect(w, []byte(req), addr)
	} else {
		// no keep-alive: new connection each request
		w.logger.Write(fmt.Sprintf("Conn (r%d): %s\n", absIdx+1, addr))

		// fire-and-forget in non-keepalive mode
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

	// apply response actions (uses absIdx for global pattern matching)
	if w.actionPatterns != nil && w.actionPatterns[absIdx] != nil {
		if err := o.applyResponseActions(w, absIdx, req, resp, status); err != nil {
			return err
		}
	}

	// close if not keepalive (HTTP/1.1 only, H2 always reuses)
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
