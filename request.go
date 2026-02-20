package main

import (
	"bufio"
	"bytes"
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

// String - reconstructs raw HTTP request from ParsedReq (used for actions/logging)
func (p *ParsedReq) String() string {
	var sb strings.Builder
	sb.WriteString(p.Method)
	sb.WriteString(" ")
	sb.WriteString(p.Path)
	sb.WriteString(" HTTP/2\r\nHost: ")
	sb.WriteString(p.Host)
	sb.WriteString("\r\n")
	for _, h := range p.Headers {
		sb.WriteString(h[0])
		sb.WriteString(": ")
		sb.WriteString(h[1])
		sb.WriteString("\r\n")
	}
	sb.WriteString("\r\n")
	sb.Write(p.Body)
	return sb.String()
}

// parseRawReq2H2 - parses HTTP/1.1 raw text into HTTP2 ParsedReq
// TREM always get request as HTTP/1.1, so pseudo-headers ARE IGNORED here!
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

	// Phase 1: Apply cached pattern values from previous request (same thread group)
	// Other thread groups can use globalStaticVals to cross Group sharing
	if w.lastPatternVals != nil {
		for k, v := range w.lastPatternVals {
			values[k] = v
		}
		w.lastPatternVals = nil // consumes it
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

// buildAndDispatch - builds request, normalizes, and dispatches to appropriate handler
// Used to consolidate the flow -> normalize -> parseHost -> dispatch for any TREM mode (sync, async and block)
// checkBarrier: if true, checks sync barrier before sending (false for subsequent combo requests)
// Returns error from any step; nil on success
func (o *Orch) buildAndDispatch(w *monkey, relIdx, absIdx int, tmpl *TemplateReq, values map[string]string, checkBarrier bool) error {
	req := buildRequest(tmpl, values)
	req = normalizeReq(req, o.blockMode)
	addr, err := parseHost(req, o.hostFlag)
	if err != nil {
		return err
	}
	if o.blockMode {
		return o.appendToBlock(w, relIdx, absIdx, req, addr)
	}
	if checkBarrier && o.mode == "sync" && o.syncBarriers[relIdx] {
		if err := o.doBarrier(w, relIdx, addr); err != nil {
			return err
		}
	}
	return o.sendReq(w, relIdx, absIdx, req, addr)
}

// processReq - unified request processor for sync/async/block modes
// relIdx = relative index within group (for patterns and barriers)
// absIdx = absolute index in reqFiles (for loading request and response actions)
func (o *Orch) processReq(w *monkey, relIdx, absIdx int) error {
	if verbose {
		w.logger.Write(fmt.Sprintf("DEBUG: valChan len=%d, localBuffer[n]=%d\n",
			len(w.valChan), len(w.localBuffer["n"])))
	}
	// Phase 1: load template and apply patterns
	tmpl, values, err := o.prepareReq(w, relIdx, absIdx)
	if err != nil {
		return err
	}

	// Phase 2: handle static mode (-u key=val)
	if o.valDist != nil && o.valDist.IsStatic() {
		k, v := o.valDist.StaticKV()
		if k != "" {
			// Only use -u value if not already set (allows regex extraction to override)
			if _, exists := values[k]; !exists {
				values[k] = v
			}
		}
	}

	// Phase 3: get non-static keys (key[0]!='_') needing values for request
	keyNames := getUniqueKeyNames(tmpl)
	var keys []string
	var needsFifo bool
	for _, key := range keyNames {
		if _, hasVal := values[key]; !hasVal {
			keys = append(keys, key)
			if len(key) > 0 && key[0] != '_' {
				needsFifo = true
			}
		}
	}

	// No additional keys needed, send directly
	if len(keys) == 0 {
		return o.buildAndDispatch(w, relIdx, absIdx, tmpl, values, true)
	}

	// Phase 4: handle FIFO mode - drain channel only if it's needed
	if needsFifo && o.valDist != nil && o.valDist.IsFifo() {
		drainChannel(w, o.fifoBlockSize)
	}

	// Phase 5: check for missing keys in FIFO buffer and globalStatic
	missing := checkMissingKeys(w.localBuffer, keys)
	if len(missing) > 0 {
		// Check if any missing key is in wait2Keys (wk=)
		hasWaitKey := false
		for _, m := range missing {
			for _, wk := range w.wait2Keys {
				if m == wk {
					hasWaitKey = true
					break
				}
			}
			if hasWaitKey {
				break
			}
		}

		if o.fifoWait || hasWaitKey {
			// Wait for keys: -fw flag OR wk= match triggers wait
			if !waitForKeys(w, keys, values, o.fifoBlockSize, o.quitChan) {
				return fmt.Errorf("FIFO timeout waiting for keys: %s", strings.Join(missing, ", "))
			}
		} else {
			for _, k := range missing {
				w.logger.Write(fmt.Sprintf("Key %s not found, sending anyway!\n", k))
			}
		}
	}

	// Phase 6: generate combinations from FIFO buffer
	combinations := generateCombinations(w.localBuffer, keys)
	if len(combinations) == 0 {
		// no values available, send as-is
		return o.buildAndDispatch(w, relIdx, absIdx, tmpl, values, true)
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

		if err := o.buildAndDispatch(w, relIdx, absIdx, tmpl, mergedVals, firstReq); err != nil {
			return err
		}
		// After first request, disable barrier check for subsequent combinations
		// In blockMode this has no effect (barrier not checked when blockMode=true)
		firstReq = false
	}

	// consume used values
	consumeValues(w.localBuffer, keys, len(combinations))

	return nil
}

// appendToBlock - accumulates request in buffer for block mode
// HTTP/1.1: raw bytes pipelining | HTTP/2: ParsedReq multiplexing
// Validates host consistency. Triggers flushBlock when buffer exceeds maxDataSize or last request.
// In keepAll mode: only accumulates, never flushes (coordinator handles flush)
func (o *Orch) appendToBlock(w *monkey, relIdx, absIdx int, req, addr string) error {
	if o.httpH2 {
		// HTTP/2 block mode: accumulate ParsedReq for multiplexing
		if !o.keepAll {
			// Normal mode: each thread has own connection
			if w.h2conn == nil {
				w.logger.Write(fmt.Sprintf("Block-Conn H2: %s\n", addr))
				if err := o.dialWithRetry(w, addr); err != nil {
					return fmt.Errorf("dial: %v", err)
				}
			} else if addr != w.connAddr {
				return fmt.Errorf("block mode: host mismatch %s vs %s. Multiplexing requires same host!", addr, w.connAddr)
			}
		}

		parsed := parseRawReq2H2(req)
		if parsed == nil {
			return fmt.Errorf("failed to parse H2 request")
		}
		w.blockH2Reqs = append(w.blockH2Reqs, parsed)
		w.blockAbsIdx = append(w.blockAbsIdx, absIdx)

		// keepAll mode: never flush here, coordinator handles it
		if o.keepAll {
			w.logger.Write(fmt.Sprintf("Queued r%d (H2 KA)\n", absIdx+1))
			return nil
		}

		isLastReq := relIdx == len(w.reqIndices)-1
		if isLastReq {
			return o.flushBlock(w)
		}

		w.logger.Write(fmt.Sprintf("Queued r%d (H2)\n", absIdx+1))
		return nil
	}

	// HTTP/1.1 block mode: accumulate raw bytes for pipelining
	if !o.keepAll {
		// Normal mode: each thread has own connection
		if len(w.blockOffsets) == 0 {
			if w.conn == nil {
				w.logger.Write(fmt.Sprintf("Block-Conn: %s\n", addr))
				if err := o.dialWithRetry(w, addr); err != nil {
					return fmt.Errorf("dial: %v", err)
				}
			}
		} else {
			// Validate same host for pipelining
			if addr != w.connAddr {
				return fmt.Errorf("block mode: host mismatch %s vs %s. Pipelining requires same host!", addr, w.connAddr)
			}
		}
	}

	// Record offsets and append request
	w.blockOffsets = append(w.blockOffsets, len(w.blockBuf))
	w.blockAbsIdx = append(w.blockAbsIdx, absIdx)
	w.blockBuf = append(w.blockBuf, req...)

	// keepAll mode: never flush here, coordinator handles it
	if o.keepAll {
		w.logger.Write(fmt.Sprintf("Queued r%d (%d bytes, KA)\n", absIdx+1, len(req)))
		return nil
	}

	isLastReq := relIdx == len(w.reqIndices)-1
	bufferFull := len(w.blockBuf) >= o.maxDataSize

	if isLastReq || bufferFull {
		return o.flushBlock(w)
	}

	w.logger.Write(fmt.Sprintf("Queued r%d (%d bytes, total %d)\n", absIdx+1, len(req), len(w.blockBuf)))
	return nil
}

// flushBlock - sends accumulated requests in single TCP write, reads responses
// HTTP/1.1: pipelining (RFC7230 FIFO order) | HTTP/2: multiplexing (correlate by streamID)
func (o *Orch) flushBlock(w *monkey) error {
	if o.httpH2 {
		// HTTP/2 block mode: multiplexed requests
		if len(w.blockH2Reqs) == 0 {
			return nil
		}

		reqCount := len(w.blockH2Reqs)
		w.logger.Write(fmt.Sprintf("Flush %d reqs (H2 multiplex)\n", reqCount))

		// Single TCP write with all requests
		streamIDs, bytesOut, err := w.h2conn.sendBatchH2(w.blockH2Reqs)
		if err != nil {
			return fmt.Errorf("H2 batch send: %v", err)
		}

		// Map streamID → index for response correlation
		streamToIdx := make(map[uint32]int, reqCount)
		for i, sid := range streamIDs {
			streamToIdx[sid] = i
		}

		// Read responses (order may vary due to H2 multiplexing!)
		for i := 0; i < reqCount; i++ {
			streamID, resp, status, bytesIn, err := w.h2conn.readResponse(0) // 0 = any stream
			if err != nil {
				return fmt.Errorf("H2 read resp %d: %v", i+1, err)
			}

			idx, ok := streamToIdx[streamID]
			if !ok {
				return fmt.Errorf("H2 unknown streamID %d", streamID)
			}
			absIdx := w.blockAbsIdx[idx]

			w.logger.Write(fmt.Sprintf("HTTP (r%d) %s\n", absIdx+1, status))
			w.prevResp = resp
			stats.ReportRequest(w.id, 0, uint32(bytesIn), 0)

			// Apply response actions if configured
			if w.actionPatterns != nil && w.actionPatterns[absIdx] != nil {
				reqStr := w.blockH2Reqs[idx].String()
				if err := o.applyResponseActions(w, absIdx, reqStr, resp, status); err != nil {
					return err
				}
			}
		}

		// Report aggregate bytes out
		stats.ReportRequest(w.id, 0, 0, uint32(bytesOut))

		// Clear buffers for next batch
		w.blockH2Reqs = w.blockH2Reqs[:0]
		w.blockAbsIdx = w.blockAbsIdx[:0]
		return nil
	}

	// HTTP/1.1 block mode: pipelined requests
	if len(w.blockBuf) == 0 {
		return nil
	}

	reqCount := len(w.blockOffsets)
	w.logger.Write(fmt.Sprintf("Flush %d reqs (%d bytes)\n", reqCount, len(w.blockBuf)))

	// Single TCP write
	bytesOut := uint32(len(w.blockBuf))
	if _, err := w.conn.Write(w.blockBuf); err != nil {
		return fmt.Errorf("block write: %v", err)
	}
	reader := bufio.NewReader(w.conn)

	// Read responses in FIFO order (HTTP/1.1 pipelining RFC7230 guarantees order)
	for i := 0; i < reqCount; i++ {
		absIdx := w.blockAbsIdx[i]
		stats.ReportRequest(w.id, 0, 0, 0)
		resp, status, _, err := readHTTP1Resp(w.id, reader)
		if err != nil {
			return fmt.Errorf("block read resp %d: %v", i+1, err)
		}

		w.logger.Write(fmt.Sprintf("HTTP (r%d) %s\n", absIdx+1, status))
		w.prevResp = resp

		// Apply response actions if configured
		if w.actionPatterns != nil && w.actionPatterns[absIdx] != nil {
			req := o.extractReq(w, i)
			if err := o.applyResponseActions(w, absIdx, req, resp, status); err != nil {
				return err
			}
		}
	}

	// Report stats (aggregate for the flush)
	stats.ReportRequest(w.id, 0, 0, bytesOut)

	// Clear buffers for next batch
	w.blockBuf = w.blockBuf[:0]
	w.blockOffsets = w.blockOffsets[:0]
	w.blockAbsIdx = w.blockAbsIdx[:0]

	return nil
}

// doGlobalFlush - flushes all monkey buffers in single TCP write (keepAll mode)
// Uses monkey[0].conn/h2conn as shared connection
func (o *Orch) doGlobalFlush() error {
	if o.httpH2 {
		return o.doGlobalFlushH2()
	}
	return o.doGlobalFlushH1()
}

// doGlobalFlushH1 - HTTP/1.1 global flush: concatenate all buffers, single write
func (o *Orch) doGlobalFlushH1() error {
	// Calculate total size
	var totalSize int
	var totalReqs int
	for _, m := range o.monkeys {
		totalSize += len(m.blockBuf)
		totalReqs += len(m.blockOffsets)
	}

	if totalSize == 0 {
		return nil
	}

	// Concatenate all buffers
	totalBuf := make([]byte, 0, totalSize)
	for _, m := range o.monkeys {
		totalBuf = append(totalBuf, m.blockBuf...)
	}

	conn := o.monkeys[0].conn
	o.monkeys[0].logger.Write(fmt.Sprintf("Global flush %d reqs (%d bytes) from %d threads\n",
		totalReqs, totalSize, len(o.monkeys)))

	// Single TCP write
	if _, err := conn.Write(totalBuf); err != nil {
		return fmt.Errorf("global flush write: %v", err)
	}

	reader := bufio.NewReader(conn)

	// Read responses in order (requests were concatenated in monkey order)
	for _, m := range o.monkeys {
		for i := 0; i < len(m.blockOffsets); i++ {
			absIdx := m.blockAbsIdx[i]
			resp, status, _, err := readHTTP1Resp(m.id, reader)
			if err != nil {
				return fmt.Errorf("global read resp m%d r%d: %v", m.id, absIdx+1, err)
			}

			m.logger.Write(fmt.Sprintf("HTTP (r%d) %s\n", absIdx+1, status))
			m.prevResp = resp

			// Apply response actions if configured
			if m.actionPatterns != nil && m.actionPatterns[absIdx] != nil {
				req := o.extractReq(m, i)
				if err := o.applyResponseActions(m, absIdx, req, resp, status); err != nil {
					return err
				}
			}
		}

		// Clear monkey's buffers
		m.blockBuf = m.blockBuf[:0]
		m.blockOffsets = m.blockOffsets[:0]
		m.blockAbsIdx = m.blockAbsIdx[:0]
	}

	stats.ReportRequest(o.monkeys[0].id, 0, 0, uint32(totalSize))
	return nil
}

// doGlobalFlushH2 - HTTP/2 global flush: encode all requests, single write
func (o *Orch) doGlobalFlushH2() error {
	// Count total requests
	var totalReqs int
	for _, m := range o.monkeys {
		totalReqs += len(m.blockH2Reqs)
	}

	if totalReqs == 0 {
		return nil
	}

	h2 := o.monkeys[0].h2conn
	if err := h2.handshake(); err != nil {
		return fmt.Errorf("H2 handshake: %v", err)
	}

	// Metadata for response correlation
	type reqMeta struct {
		monkey   *monkey
		idx      int // index in monkey's blockH2Reqs
		absIdx   int
		streamID uint32
	}
	metas := make([]reqMeta, 0, totalReqs)
	streamToMeta := make(map[uint32]*reqMeta, totalReqs)

	// Encode all requests into single buffer
	var buf bytes.Buffer
	for _, m := range o.monkeys {
		for i, req := range m.blockH2Reqs {
			streamID := h2.streamID
			h2.streamID += 2
			h2.encodeReqFrames(&buf, req, streamID)

			meta := reqMeta{
				monkey:   m,
				idx:      i,
				absIdx:   m.blockAbsIdx[i],
				streamID: streamID,
			}
			metas = append(metas, meta)
			streamToMeta[streamID] = &metas[len(metas)-1]
		}
	}

	o.monkeys[0].logger.Write(fmt.Sprintf("Global flush %d reqs (H2) from %d threads\n",
		totalReqs, len(o.monkeys)))

	// Single TCP write
	bytesOut := buf.Len()
	if _, err := h2.conn.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("H2 global flush write: %v", err)
	}

	// Read responses (may arrive out of order)
	for i := 0; i < totalReqs; i++ {
		streamID, resp, status, bytesIn, err := h2.readResponse(0)
		if err != nil {
			return fmt.Errorf("H2 global read resp %d: %v", i+1, err)
		}

		meta, ok := streamToMeta[streamID]
		if !ok {
			return fmt.Errorf("H2 unknown streamID %d", streamID)
		}

		m := meta.monkey
		m.logger.Write(fmt.Sprintf("HTTP (r%d) %s\n", meta.absIdx+1, status))
		m.prevResp = resp
		stats.ReportRequest(m.id, 0, uint32(bytesIn), 0)

		// Apply response actions if configured
		if m.actionPatterns != nil && m.actionPatterns[meta.absIdx] != nil {
			reqStr := m.blockH2Reqs[meta.idx].String()
			if err := o.applyResponseActions(m, meta.absIdx, reqStr, resp, status); err != nil {
				return err
			}
		}
	}

	// Clear all monkey buffers
	for _, m := range o.monkeys {
		m.blockH2Reqs = m.blockH2Reqs[:0]
		m.blockAbsIdx = m.blockAbsIdx[:0]
	}

	stats.ReportRequest(o.monkeys[0].id, 0, 0, uint32(bytesOut))
	return nil
}

// extractReq - extracts request i from blockBuf using offsets
func (o *Orch) extractReq(w *monkey, i int) string {
	start := w.blockOffsets[i]
	end := len(w.blockBuf)
	if i+1 < len(w.blockOffsets) {
		end = w.blockOffsets[i+1]
	}
	return string(w.blockBuf[start:end])
}

// sendFireAndForget - sends request without waiting for response
// Used when patterns[relIdx] is empty, indicating no response processing needed
func (o *Orch) sendFireAndForget(w *monkey, req, addr string, absIdx int) error {
	w.logger.Write(fmt.Sprintf("Fire&Forget (r%d)\n", absIdx+1))
	bytesOut := uint32(len(req))

	if o.keepAlive || o.httpH2 {
		// keepalive mode: use existing connection
		if o.httpH2 {
			parsedReq := parseRawReq2H2(req)
			if parsedReq == nil {
				return fmt.Errorf("failed to parse request for fire-and-forget")
			}
			if _, _, err := w.h2conn.sendReqH2(parsedReq, 0, false); err != nil {
				return err
			}
		} else {
			if _, err := w.conn.Write([]byte(req)); err != nil {
				return err
			}
		}
		o.closeWorkerConn(w)
	} else {
		// non-keepalive mode: create new connection
		conn, err := o.dialNewConn(addr)
		if err != nil {
			return err
		}
		_, err = conn.Write([]byte(req))
		conn.Close()
		if err != nil {
			return err
		}
	}

	stats.ReportRequest(w.id, 0, 0, bytesOut)
	w.prevResp = ""
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
	fireAndForget := w.patterns != nil && relIdx < len(w.patterns) && w.patterns[relIdx] != nil && len(w.patterns[relIdx]) == 0
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

		if fireAndForget {
			return o.sendFireAndForget(w, req, addr, absIdx)
		}

		resp, status, err = o.sendWithReconnect(w, []byte(req), addr)
	} else {
		// no keep-alive: new connection each request
		w.logger.Write(fmt.Sprintf("Conn (r%d): %s\n", absIdx+1, addr))

		if fireAndForget {
			return o.sendFireAndForget(w, req, addr, absIdx)
		}

		resp, status, err = o.sendWithRetry(w, []byte(req), addr)
	}

	if err != nil {
		return err
	}

	w.logger.Write(fmt.Sprintf("HTTP (r%d) %s\n", absIdx+1, status))
	w.prevResp = resp
	// Extract patterns and cache for next request
	if w.patterns != nil && relIdx < len(w.patterns) && len(w.patterns[relIdx]) > 0 {
		patternVals, err := applyPatterns(w.patterns[relIdx], resp, w.logger)
		if err != nil {
			return err
		}
		w.lastPatternVals = patternVals // cache para prepareReq
	}

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

	// Filter comment lines (# cmt)
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

		// Empty line: read response but skip regex match
		if line == "" {
			patterns[i] = nil
			continue
		} else if line == ":" {
			// Line with only : is fire-and-forget marker, keep empty slice for indexing
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

	w.logger.Write(fmt.Sprintf("RA matched (r%d): %s\n", idx+1, m))

	// Execute actions in order
	for _, action := range ap.actions {
		switch action.actionType {
		case ActionPrintAll:
			// Broadcast message to all threads
			msg := fmt.Sprintf("\n>>> RA (r%d) T%d: %s <<<\n", idx+1, w.id+1, action.arg)
			o.uiManager.BroadcastMessage(msg)

		case ActionPrintThread:
			// Print to this thread only
			w.logger.Write(fmt.Sprintf(">>> RA: %s <<<\n", action.arg))

		case ActionPauseThread:
			// Pause current thread
			o.pauseThread(w.id)
			o.checkPause(w)

		case ActionPauseAllThreads:
			// Pause ALL threads
			o.pauseAllThreads()
			o.checkPause(w)

		case ActionSaveReq:
			// Save request that generated match
			if err := os.WriteFile(action.arg, []byte(req), 0644); err != nil {
				w.logger.Write(fmt.Sprintf("RA save error: %v\n", err))
			} else {
				w.logger.Write(fmt.Sprintf("RA saved req to: %s\n", action.arg))
			}

		case ActionSaveResp:
			// Save response that generated match
			if err := os.WriteFile(action.arg, []byte(resp), 0644); err != nil {
				w.logger.Write(fmt.Sprintf("RA save error: %v\n", err))
			} else {
				w.logger.Write(fmt.Sprintf("RA saved resp to: %s\n", action.arg))
			}

		case ActionSaveAll:
			// Save both request and response
			content := fmt.Sprintf("=== REQUEST ===\n%s\n\n=== RESPONSE ===\n%s", req, resp)
			if err := os.WriteFile(action.arg, []byte(content), 0644); err != nil {
				w.logger.Write(fmt.Sprintf("RA save error: %v\n", err))
			} else {
				w.logger.Write(fmt.Sprintf("RA saved req+resp to: %s\n", action.arg))
			}

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
