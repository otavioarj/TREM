package main

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Action types for response actions
const (
	ActionPrintThread = iota // pt - print + pause thread
	ActionPrintAll           // pa - print all + pause all
	ActionSaveReq            // sre - save request + pause thread
	ActionSaveResp           // srp - save response + pause thread
	ActionSaveAll            // sa - save both + pause thread
	ActionExit               // e - graceful exit
)

type respAction struct {
	actionType int
	arg        string // msg or path (empty for 'e')
}

type actionPattern struct {
	re      *regexp.Regexp
	actions []respAction
}

// Pre-compiled regex for parseHost
var hostHeaderRe = regexp.MustCompile(`(?im)^Host:\s*([^:\r\n]+)(?::(\d+))?`)

// Pre-compiled regex for extracting $key$ patterns
var keyPatternRe = regexp.MustCompile(`\$([^$]+)\$`)

// drainChannel - drains messages from channel into localBuffer (non-blocking)
// limit=0 means unlimited, otherwise drains at most 'limit' messages
func drainChannel(w *monkey, limit int) {
	if w.valChan == nil {
		return
	}
	count := 0
	for {
		if limit > 0 && count >= limit {
			return // reached limit
		}
		select {
		case msg := <-w.valChan:
			for k, v := range msg {
				w.localBuffer[k] = append(w.localBuffer[k], v)
			}
			count++
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
func consumeValues(buffer map[string][]string, keys []string, count int) {
	for _, k := range keys {
		if len(buffer[k]) > 0 && k[1] != '_' {
			if count >= len(buffer[k]) {
				delete(buffer, k)
			} else {
				buffer[k] = buffer[k][count:]
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

func FormatConfig(threads, delay, loopStart, loopTimes, cliHello, fbck int,
	keepAlive, verbose, httpVer bool,
	proxy, host, mtls, mode, univ string) string {
	var lines []string
	lines = append(lines, fmt.Sprintf("HTTP2: %v. Verbose: %v. Threads: %d. FIFO Block: %d. ", httpVer, verbose, threads, fbck))
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

// parseIndexList - parses "1,2,4" into map[int]bool (0-based indices)
// Returns error if empty or invalid
func parseIndexList(s string) (map[int]bool, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("index list cannot be empty")
	}
	indices := make(map[int]bool)
	parts := strings.Split(s, ",")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		idx, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("invalid index: %s", p)
		}
		if idx < 1 {
			return nil, fmt.Errorf("index must be >= 1: %d", idx)
		}
		indices[idx-1] = true // convert to 0-based
	}
	if len(indices) == 0 {
		return nil, errors.New("index list cannot be empty")
	}
	return indices, nil
}

// loadActionPatterns - loads and parses -ra file
// Format per line: 1,2,4:regex`:action1,action2
// Returns map[idx]*actionPattern (one pattern per idx)
func loadActionPatterns(path string) (map[int]*actionPattern, error) {
	if path == "" {
		return nil, nil
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read action file: %v", err)
	}

	result := make(map[int]*actionPattern)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")

	for lineNum, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Parse: indices:regex`:actions
		// Find first backtick for indices
		btIdx := strings.IndexByte(line, ':')
		if btIdx < 1 {
			return nil, fmt.Errorf("line %d: missing indices before regex", lineNum+1)
		}

		// Parse indices
		indices, err := parseIndexList(line[:btIdx])
		if err != nil {
			return nil, fmt.Errorf("line %d: %v", lineNum+1, err)
		}

		// Find closing backtick and colon
		rest := line[btIdx+1:]
		btEnd := strings.IndexByte(rest, '`')
		if btEnd < 1 {
			return nil, fmt.Errorf("line %d: missing closing backtick for regex", lineNum+1)
		}

		regexStr := rest[:btEnd]
		rest = rest[btEnd+1:]

		if len(rest) < 2 || rest[0] != ':' {
			return nil, fmt.Errorf("line %d: missing ':' after regex", lineNum+1)
		}
		rest = rest[1:] // skip ':'

		// Compile regex
		re, err := regexp.Compile(regexStr)
		if err != nil {
			return nil, fmt.Errorf("line %d: invalid regex: %v", lineNum+1, err)
		}

		// Parse actions
		actions, err := parseActions(rest)
		if err != nil {
			return nil, fmt.Errorf("line %d: %v", lineNum+1, err)
		}

		// Create pattern
		ap := &actionPattern{
			re:      re,
			actions: actions,
		}

		// Assign to each index (error if duplicate)
		for idx := range indices {
			if result[idx] != nil {
				return nil, fmt.Errorf("line %d: duplicate action for index %d", lineNum+1, idx+1)
			}
			result[idx] = ap
		}
	}

	return result, nil
}

// parseActions - parses "pt("msg"),sre("/path"),e" into []respAction
func parseActions(s string) ([]respAction, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("actions cannot be empty")
	}

	var actions []respAction
	// Split by comma, but careful with commas inside quotes
	// Use state machine approach
	i := 0
	for i < len(s) {
		// Skip whitespace
		for i < len(s) && s[i] == ' ' {
			i++
		}
		if i >= len(s) {
			break
		}

		// Find action type
		var actionType int
		var needsArg bool
		var consumed int

		switch {
		case strings.HasPrefix(s[i:], "pt("):
			actionType = ActionPrintThread
			needsArg = true
			consumed = 3
		case strings.HasPrefix(s[i:], "pa("):
			actionType = ActionPrintAll
			needsArg = true
			consumed = 3
		case strings.HasPrefix(s[i:], "sre("):
			actionType = ActionSaveReq
			needsArg = true
			consumed = 4
		case strings.HasPrefix(s[i:], "srp("):
			actionType = ActionSaveResp
			needsArg = true
			consumed = 4
		case strings.HasPrefix(s[i:], "sa("):
			actionType = ActionSaveAll
			needsArg = true
			consumed = 3
		case s[i] == 'e':
			actionType = ActionExit
			needsArg = false
			consumed = 1
		default:
			return nil, fmt.Errorf("unknown action at position %d: %s", i, s[i:])
		}

		i += consumed

		var arg string
		if needsArg {
			// Expect ("...")
			if i >= len(s) || s[i] != '"' {
				return nil, fmt.Errorf("expected '\"' after action at position %d", i)
			}
			i++ // skip opening "

			// Find closing ")
			endQuote := strings.Index(s[i:], "\")")
			if endQuote < 0 {
				return nil, fmt.Errorf("missing closing \"\") for action")
			}
			arg = s[i : i+endQuote]
			i += endQuote + 2 // skip content + ")
		}

		actions = append(actions, respAction{
			actionType: actionType,
			arg:        arg,
		})

		// Skip whitespace and comma
		for i < len(s) && s[i] == ' ' {
			i++
		}
		if i < len(s) && s[i] == ',' {
			i++
		}
	}

	if len(actions) == 0 {
		return nil, errors.New("no valid actions found")
	}

	return actions, nil
}

// saveToFile - saves content to file with _idx_epoch suffix
func saveToFile(basePath string, idx int, content string) error {
	epoch := time.Now().Unix()
	var finalPath string

	dotIdx := strings.LastIndexByte(basePath, '.')
	if dotIdx > 0 {
		// Insert before extension: /path/file.txt -> /path/file_idx_epoch.txt
		finalPath = fmt.Sprintf("%s_%d_%d%s", basePath[:dotIdx], idx, epoch, basePath[dotIdx:])
	} else {
		// No extension: /path/file -> /path/file_idx_epoch
		finalPath = fmt.Sprintf("%s_%d_%d", basePath, idx, epoch)
	}

	return os.WriteFile(finalPath, []byte(content), 0644)
}

// formatH2ResponseAsH1 - converts H2 response headers to HTTP/1.1 format
func formatH2ResponseAsH1(resp string, status string) string {
	// H2 response from readResponse is already: "header: value\r\n...\r\n\r\nbody"
	// Just prepend HTTP/1.1 status line
	return fmt.Sprintf("HTTP/1.1 %s \r\n%s", status, resp)
}
