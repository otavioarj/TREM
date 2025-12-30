package main

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Pre-compiled regex for parseHost
var hostHeaderRe = regexp.MustCompile(`(?im)^Host:\s*([^:\r\n]+)(?::(\d+))?`)

// Pre-compiled regex for extracting $key$ patterns
var keyPatternRe = regexp.MustCompile(`\$([^$]+)\$`)

// drainChannel - drains all available messages from channel into localBuffer (non-blocking)
func drainChannel(w *monkey) {
	if w.valChan == nil {
		return
	}
	for {
		select {
		case msg := <-w.valChan:
			for k, v := range msg {
				w.localBuffer[k] = append(w.localBuffer[k], v)
			}
		default:
			return // no more messages
		}
	}
}

// waitForKeys - blocks until all required keys are available in localBuffer
// Emits periodic messages about missing keys
func waitForKeys(w *monkey, keys []string, done <-chan struct{}) bool {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var prints []string
	// Wait for max tries, or ticker*max_wait seconds
	max_wait := 5
	wait_idx := 1
	for {
		// Check if all keys present
		var missing []string
		for _, k := range keys {
			if len(w.localBuffer[k]) == 0 {
				missing = append(missing, k)
			}
		}
		if len(missing) == 0 {
			return true
		}

		// Wait for more data or emit status
		select {
		case msg, ok := <-w.valChan:
			if !ok {
				return false
			}
			for k, v := range msg {
				w.localBuffer[k] = append(w.localBuffer[k], v)
			}
		case <-ticker.C:
			if wait_idx == max_wait {
				w.logger.Write(fmt.Sprintf("Giving up FIFO keys: %v\n", missing))
				return false
			}
			wait_idx++
			for p := range missing {
				if len(prints) > p && missing[p] != prints[p] {
					w.logger.Write(fmt.Sprintf("Waiting FIFO keys: %v\n", missing))
				}
				prints = missing
			}
		case <-done:
			return false
		}
	}
}

// checkMissingKeys - returns keys present in request but not in localBuffer
func checkMissingKeys(buffer map[string][]string, keys []string) []string {
	var missing []string
	for _, k := range keys {
		if len(buffer[k]) == 0 {
			missing = append(missing, k)
		}
	}
	return missing
}

// extractKeys - extracts all $key$ patterns from request string
func extractKeys(req string) []string {
	matches := keyPatternRe.FindAllStringSubmatch(req, -1)
	keys := make([]string, 0, len(matches))
	for _, m := range matches {
		key := "$" + m[1] + "$"
		// Linear search - faster for small N due to cache
		found := false
		for _, k := range keys {
			if k == key {
				found = true
				break
			}
		}
		if !found {
			keys = append(keys, key)
		}
	}
	return keys
}

// generateCombinations - generates cartesian product of values for keys
// Returns list of maps, each map is one combination; which is one request later
func generateCombinations(buffer map[string][]string, keys []string) []map[string]string {
	if len(keys) == 0 {
		return nil
	} // Filter keys that exist in buffer
	var validKeys []string
	for _, k := range keys {
		if len(buffer[k]) > 0 {
			validKeys = append(validKeys, k)
		}
	}

	if len(validKeys) == 0 {
		return nil
	}

	// Calculate total combinations
	total := 1
	for _, k := range validKeys {
		total *= len(buffer[k])
	}
	combinations := make([]map[string]string, total)
	for i := range combinations {
		combinations[i] = make(map[string]string)
	}

	// Fill combinations (cartesian product)
	repeat := total
	for _, k := range validKeys {
		vals := buffer[k]
		repeat /= len(vals)
		for i := 0; i < total; i++ {
			combinations[i][k] = vals[(i/repeat)%len(vals)]
		}
	}

	return combinations
}

// consumeValues - removes used values from localBuffer
func consumeValues(buffer map[string][]string, keys []string) {
	for _, k := range keys {
		// This is the real magic :) All keys that starts with '_', i.e, $_key$
		// Never is consumed, thus, will live for all -xt loops
		// Multiples values for $_key$ WILL generate combinations, so all values
		// WILL be a request!!
		if len(buffer[k]) > 0 && k[1] != '_' {
			buffer[k] = buffer[k][1:] // remove it first
			if len(buffer[k]) == 0 {
				delete(buffer, k)
			}
		}
	}
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
	lines = append(lines, fmt.Sprintf("HTTP2: %v. Verbose: %v. Threads: %d. TLS Tout: %dms. ", httpVer, verbose, threads, tlsTimeout))
	lines = append(lines, fmt.Sprintf("Keep-alive: %v. \nDelay: %dms.  Mode: %s.", keepAlive, delay, mode))
	loopTimesStr := "∞"

	if loopTimes != 0 {
		loopTimesStr = fmt.Sprintf("%d", loopTimes)
	}
	lines = append(lines, fmt.Sprintf(" LoopStart: %d. LoopTimes: %s. CliHello: %s.\n", loopStart, loopTimesStr,
		clientHelloNames[cliHello]))

	if mtls != "" {
		lines = append(lines, fmt.Sprintf("ClientCert: %s\n", mtls))
	}
	if proxy != "" {
		lines = append(lines, fmt.Sprintf("Proxy: %s", proxy))
	}
	if host != "" {
		lines = append(lines, fmt.Sprintf("Forced Host: %s", host))
	}

	return strings.Join(lines, "")
}

func parseHost(req, override string) (string, error) {
	if override != "" {
		return override, nil
	}

	for _, line := range strings.Split(req, "\n") {
		if m := hostHeaderRe.FindStringSubmatch(line); m != nil {
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

// decodeExtracted - decodes URL (%XX) and HTML (&entity;)
func decodeExtracted(s string) string {
	if !strings.ContainsAny(s, "%&") {
		return s
	}

	var sb strings.Builder
	sb.Grow(len(s))
	i := 0

	for i < len(s) {
		switch s[i] {
		case '%':
			// URL decode: %XX
			if i+2 < len(s) {
				if b, ok := hexDecode(s[i+1], s[i+2]); ok {
					sb.WriteByte(b)
					i += 3
					continue
				}
			}
		case '&':
			// HTML entity: &name; or &#num; or &#xHH;
			if end := strings.IndexByte(s[i:], ';'); end > 1 && end < 10 {
				entity := s[i+1 : i+end]
				if c, ok := decodeHTMLEntity(entity); ok {
					sb.WriteString(c)
					i += end + 1
					continue
				}
			}
		}
		sb.WriteByte(s[i])
		i++
	}

	return sb.String()
}

// hexDecode - decodes two hex chars to byte
func hexDecode(h1, h2 byte) (byte, bool) {
	v1, ok1 := hexVal(h1)
	v2, ok2 := hexVal(h2)
	if ok1 && ok2 {
		return v1<<4 | v2, true
	}
	return 0, false
}

func hexVal(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// decodeHTMLEntity - decodes common HTML entities
func decodeHTMLEntity(entity string) (string, bool) {
	// Numeric: &#65; or &#x41;
	if len(entity) > 1 && entity[0] == '#' {
		var n int64
		if entity[1] == 'x' || entity[1] == 'X' {
			for _, c := range entity[2:] {
				if v, ok := hexVal(byte(c)); ok {
					n = n<<4 | int64(v)
				} else {
					return "", false
				}
			}
		} else {
			for _, c := range entity[1:] {
				if c >= '0' && c <= '9' {
					n = n*10 + int64(c-'0')
				} else {
					return "", false
				}
			}
		}
		if n > 0 && n < 0x10FFFF {
			return string(rune(n)), true
		}
		return "", false
	}

	// Named entities (common ones)
	switch entity {
	case "amp":
		return "&", true
	case "lt":
		return "<", true
	case "gt":
		return ">", true
	case "quot":
		return "\"", true
	case "apos":
		return "'", true
	case "nbsp":
		return " ", true
	}
	return "", false
}

// applyVals - replaces all keys in req with values from map
func applyVals(req string, vals map[string]string) string {
	for k, v := range vals {
		req = strings.ReplaceAll(req, k, v)
	}
	return req
}

// parseProxyAddr - simply parses a URL to extract just host:port
// avoiding the use of net/url just for that :)
func parseProxyAddr(proxy string) (string, error) {
	// Remove scheme (http:// or https://)
	if idx := strings.Index(proxy, "://"); idx >= 0 {
		proxy = proxy[idx+3:]
	} else {
		return "", fmt.Errorf("Invalid proxy scheme!")
	}
	// Remove path if needed
	if idx := strings.IndexByte(proxy, '/'); idx >= 0 {
		proxy = proxy[:idx]
	}
	return proxy, nil
}
