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
		}
	}

	return req, nil
}

// processReq - sync mode: handles single req in chain with FIFO combinations
func (o *Orch) processReq(w *monkey, idx int, addr string) error {
	baseReq, err := o.prepareReqBase(w, idx)
	if err != nil {
		return err
	}

	// Handle static mode
	if o.valDist.IsStatic() {
		k, v := o.valDist.StaticKV()
		req := strings.ReplaceAll(baseReq, k, v)
		return o.sendSyncReq(w, idx, req, addr)
	}
	// Handle FIFO mode
	// Drain channel first
	drainChannel(w)

	// Extract keys needed from request
	keys := extractKeys(baseReq)
	if len(keys) == 0 {
		// No FIFO keys, just send
		return o.sendSyncReq(w, idx, baseReq, addr)
	}
	// Check for missing keys now
	missing := checkMissingKeys(w.localBuffer, keys)

	if len(missing) > 0 {
		if o.fifoWait {
			// Block waiting, waitForKeys will emit periodic messages
			if !waitForKeys(w, keys, o.quitChan) {
				w.logger.Write("FIFO closed while waiting for keys")
				return nil
			}
		} else {
			// Warn and continue without substitution
			for _, k := range missing {
				w.logger.Write(fmt.Sprintf("Key %s not found in FIFO, sending anyway!!\n", k))
			}
		}
	}

	// Generate combinations: each key creates=value a new request
	//  PER original requestN.txt file!
	combinations := generateCombinations(w.localBuffer, keys)
	if len(combinations) == 0 {
		// No values available, send as-is (will likely fail regex check)
		return o.sendSyncReq(w, idx, baseReq, addr)
	}
	// Send request for each combination
	for _, combo := range combinations {
		req := baseReq
		for k, v := range combo {
			req = strings.ReplaceAll(req, k, v)
			w.logger.Write(fmt.Sprintf("Val %s: %s\n", k, v))
		}

		if err := o.sendSyncReq(w, idx, req, addr); err != nil {
			return err
		}
	}
	// Consume used values
	consumeValues(w.localBuffer, keys)
	return nil
}

// sendSyncReq - sends single request in sync mode
func (o *Orch) sendSyncReq(w *monkey, idx int, req string, addr string) error {
	req = normalizeRequest(req)
	// Save loop start req if this is the loop start point
	if o.loopStart > 0 && idx == o.loopStart-1 {
		w.loopStartReq = req
	}

	var resp, status string
	var err error
	if o.keepAlive {
		resp, status, err = o.sendWithReconnect(w, []byte(req), addr)
	} else {
		resp, status, err = o.sendWithRetry(w, []byte(req), addr)
	}

	if err != nil {
		return err
	}

	w.logger.Write(fmt.Sprintf("HTTP %s\n", status))
	w.prevResp = resp
	return nil
}

// processReqAsync - async mode: process req with FIFO combinations
func (o *Orch) processReqAsync(w *monkey, idx int) error {
	baseReq, err := o.prepareReqBase(w, idx)
	if err != nil {
		return err
	}

	// Handle static mode
	if o.valDist.IsStatic() {
		k, v := o.valDist.StaticKV()
		req := strings.ReplaceAll(baseReq, k, v)
		return o.sendAsyncReq(w, idx, req)
	}
	// Handle FIFO mode
	// Drain channel first
	drainChannel(w)

	// Extract keys needed from request
	keys := extractKeys(baseReq)
	if len(keys) == 0 {
		// No FIFO keys, just send
		return o.sendAsyncReq(w, idx, baseReq)
	}

	// Check for missing keys
	missing := checkMissingKeys(w.localBuffer, keys)
	if len(missing) > 0 {
		if o.fifoWait {
			// Block waiting, waitForKeys will emit periodic messages
			if !waitForKeys(w, keys, o.quitChan) {
				w.logger.Write("FIFO closed while waiting for keys")
				return nil
			}
		} else {
			// Warn and continue without substitution
			for _, k := range missing {
				w.logger.Write(fmt.Sprintf("Key %s not found in FIFO, sending anyway\n", k))
			}
		}
	}
	// Generate combinations: each key creates=value a new request
	//  PER original requestN.txt file!
	combinations := generateCombinations(w.localBuffer, keys)
	if len(combinations) == 0 {
		// No values available, send as-is (will likely fail regex check)
		return o.sendAsyncReq(w, idx, baseReq)
	}

	// Send request for each combination
	for _, combo := range combinations {
		req := baseReq
		for k, v := range combo {
			req = strings.ReplaceAll(req, k, v)
			w.logger.Write(fmt.Sprintf("Val %s: %s\n", k, v))
		}

		if err := o.sendAsyncReq(w, idx, req); err != nil {
			return err
		}
	}

	// Consume used values
	consumeValues(w.localBuffer, keys)
	return nil
}

// sendAsyncReq - sends single request in async mode
func (o *Orch) sendAsyncReq(w *monkey, idx int, req string) error {
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

	resp, status, err := o.sendWithRetry(w, []byte(req), addr)
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
