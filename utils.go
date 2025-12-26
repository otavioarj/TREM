package main

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

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
			if verbose {
				w.logger.Write(fmt.Sprintf("[V] regex no match, pattern: %s\n", p.keyword))
			}
			return fmt.Errorf("regex did not match for: %s", p.keyword)
		}
		req = strings.ReplaceAll(req, "$"+p.keyword+"$", m[1])
		w.logger.Write(fmt.Sprintf("Matched %s: %s\n", p.keyword, m[1]))
	}

	// Apply values from FIFO or static
	vals := o.valDist.Get()
	if len(vals) > 0 {
		req = applyVals(req, vals)
	}

	req = normalizeRequest(req)

	// Save loop start req if this is the loop start point
	if o.loopStart > 0 && idx == o.loopStart-1 {
		w.loopStartReq = req
	}

	var resp, status string
	if o.keepAlive {
		// Reuse connection with reconnect on error
		resp, status, err = o.sendWithReconnect(w, []byte(req), addr)
	} else {
		// New connection per req with retry
		resp, status, err = o.sendWithRetry(w, []byte(req), addr)
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
				if verbose {
					w.logger.Write(fmt.Sprintf("[V] regex no match, pattern: %s\n", p.keyword))
				}
				//w.logger.Write(w.prevResp+"\n")
				return fmt.Errorf("regex did not match for: %s", p.keyword)
			}
			req = strings.ReplaceAll(req, "$"+p.keyword+"$", m[1])
			w.logger.Write(fmt.Sprintf("Matched %s: %s\n", p.keyword, m[1]))
		}
	}

	// Apply values from FIFO or static
	vals := o.valDist.Get()
	if len(vals) > 0 {
		req = applyVals(req, vals)
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

	// Send with retry
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
			// Use pre-compiled regex
			line = httpVersionRe.ReplaceAllString(line, "HTTP/1.1")
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

func FormatConfig(threads, delay, loopStart, loopTimes, cliHello, tlsTimeout int,
	keepAlive, verbose, httpVer bool,
	proxy, host, mtls, mode, univ string) string {
	var lines []string
	lines = append(lines, fmt.Sprintf("HTTP/%v Verbose: %v Threads: %d TLS t.o: %dms\n", httpVer, verbose, threads, tlsTimeout))
	lines = append(lines, fmt.Sprintf("Keep-alive: %v Delay: %dms  Mode: %s\n", keepAlive, delay, mode))
	loopTimesStr := "∞"

	if loopTimes != 0 {
		loopTimesStr = fmt.Sprintf("%d", loopTimes)
	}
	lines = append(lines, fmt.Sprintf("LoopStart: %d LoopTimes: %s CliHello: %s\n", loopStart, loopTimesStr,
		clientHelloNames[cliHello]))

	if mtls != "" {
		lines = append(lines, fmt.Sprintf("ClientCert: %s ", mtls))
	}
	if proxy != "" {
		lines = append(lines, fmt.Sprintf("Proxy: %s ", proxy))
	}
	if host != "" {
		lines = append(lines, fmt.Sprintf("Forced Host: %s ", host))
	}
	if univ != "" {
		lines = append(lines, fmt.Sprintf("Univ.: %s ", univ))
	}
	lines = append(lines, "\n")
	return strings.Join(lines, "")
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
