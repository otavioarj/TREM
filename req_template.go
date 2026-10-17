package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// KeyInfo - placeholder location in template
type KeyInfo struct {
	Name  string // key name without $
	Start int    // offset of opening '$'
	End   int    // offset after closing '$'
}

// TemplateReq - parsed request template with pre-extracted keys
type TemplateReq struct {
	Raw   string    // original request content
	Keys  []KeyInfo // placeholders sorted by offset
	Names []string  // unique keys in first-use order
}

// templateCache - global cache for parsed templates (path → *TemplateReq)
var templateCache sync.Map

// parseTemplate - extracts all $...$ keys from raw request
// Returns TemplateReq with keys sorted by offset ascending
func parseTemplate(raw string) *TemplateReq {
	tmpl := &TemplateReq{
		Raw:  raw,
		Keys: make([]KeyInfo, 0, 8),
	}
	seen := make(map[string]bool)

	i := 0
	for i < len(raw) {
		start := strings.IndexByte(raw[i:], '$')
		if start < 0 {
			break
		}
		start += i

		end := strings.IndexByte(raw[start+1:], '$')
		if end < 0 {
			break
		}
		end += start + 1

		name := raw[start+1 : end]

		// key must be non-empty and not contain invalid chars
		if len(name) > 0 && !strings.ContainsAny(name, "$ \t\r\n") {
			tmpl.Keys = append(tmpl.Keys, KeyInfo{
				Name:  name,
				Start: start,
				End:   end + 1,
			})
			if !seen[name] {
				seen[name] = true
				tmpl.Names = append(tmpl.Names, name)
			}
		}

		i = end + 1
	}

	sort.Slice(tmpl.Keys, func(i, j int) bool {
		return tmpl.Keys[i].Start < tmpl.Keys[j].Start
	})

	return tmpl
}

// getTemplate - returns cached template or parses and caches new one
func getTemplate(path string, raw string) *TemplateReq {
	if cached, ok := templateCache.Load(path); ok {
		return cached.(*TemplateReq)
	}

	tmpl := parseTemplate(raw)
	templateCache.Store(path, tmpl)
	return tmpl
}

// loadTemplates - loads fixed request files once for all groups and workers.
func loadTemplates(paths []string) ([]*TemplateReq, error) {
	tmpls := make([]*TemplateReq, len(paths))
	for i, path := range paths {
		if cached, ok := templateCache.Load(path); ok {
			tmpls[i] = cached.(*TemplateReq)
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		tmpls[i] = getTemplate(path, string(raw))
	}
	return tmpls, nil
}

// buildRequest - substitutes all keys in single pass using pre-parsed template
// values: map[keyName]value for substitution
// Returns request with substitutions applied (not yet normalized)
func buildRequest(tmpl *TemplateReq, values map[string]string) string {
	if len(tmpl.Keys) == 0 {
		return tmpl.Raw
	}

	// estimate final size
	growth := 0
	for _, k := range tmpl.Keys {
		if v, ok := values[k.Name]; ok {
			growth += len(v) - (k.End - k.Start)
		}
	}
	var sb strings.Builder
	sb.Grow(len(tmpl.Raw) + growth)

	prevEnd := 0
	for _, k := range tmpl.Keys {
		sb.WriteString(tmpl.Raw[prevEnd:k.Start])

		if v, ok := values[k.Name]; ok {
			sb.WriteString(v)
		} else {
			// keep placeholder if value not found
			sb.WriteString(tmpl.Raw[k.Start:k.End])
		}
		prevEnd = k.End
	}

	sb.WriteString(tmpl.Raw[prevEnd:])
	return sb.String()
}

// readReqLine - accepts CRLF, LF or CR without scanning the body
func readReqLine(s string) (string, string) {
	i := strings.IndexAny(s, "\r\n")
	if i < 0 {
		return s, ""
	}
	n := i + 1
	if s[i] == '\r' && n < len(s) && s[n] == '\n' {
		n++
	}
	return s[:i], s[n:]
}

// splitReqHdr - separates headers at the first empty line, preserving body bytes!
func splitReqHdr(raw string) (line, hdr, body string) {
	line, hdr = readReqLine(raw)
	rest := hdr
	for rest != "" {
		prev := rest
		var h string
		h, rest = readReqLine(rest)
		if h == "" {
			return line, hdr[:len(hdr)-len(prev)], rest
		}
	}
	return line, hdr, ""
}

// hasToken - matches a complete comma-separated header token.
func hasToken(s, token string) bool {
	for s != "" {
		var part string
		part, s, _ = strings.Cut(s, ",")
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

// normalizeReq - normalizes headers and length after substitutions, preserving body bytes.
// - ensures HTTP/1.1 in request line
// - normalizes line endings to \r\n
// - calculates and sets Content-Length
func normalizeReq(req string, blockMode bool) string {
	line, hdr, body := splitReqHdr(req)
	line = strings.TrimRight(line, " \t")
	i := strings.LastIndexAny(line, " \t")
	if i >= 0 && strings.IndexAny(line[:i], " \t") >= 0 && strings.HasPrefix(line[i+1:], "HTTP/") {
		line = line[:i+1] + "HTTP/1.1"
	} else {
		line += " HTTP/1.1"
	}
	headers := make([]string, 0, 16)
	hasLen, hasTE, hasConn := false, false, false
	for hdr != "" {
		var h string
		h, hdr = readReqLine(hdr)
		key, val, ok := strings.Cut(h, ":")
		if ok {
			key = strings.TrimSpace(key)
			if strings.EqualFold(key, "Content-Length") {
				hasLen = true
			}
			if strings.EqualFold(key, "Transfer-Encoding") {
				hasTE = true
			}
			if blockMode && strings.EqualFold(key, "Connection") {
				hasConn = true
				if hasToken(val, "close") {
					tokens := strings.Split(val, ",")
					for i, token := range tokens {
						tokens[i] = strings.TrimSpace(token)
						if strings.EqualFold(tokens[i], "close") {
							tokens[i] = "keep-alive"
						}
					}
					h = h[:strings.IndexByte(h, ':')+1] + " " + strings.Join(tokens, ", ")
				}
			}
		}
		headers = append(headers, h)
	}
	if blockMode && !hasConn {
		headers = append(headers, "Connection: keep-alive")
	}
	if !hasTE {
		val := strconv.Itoa(len(body))
		if hasLen {
			for i, h := range headers {
				key, _, ok := strings.Cut(h, ":")
				if ok && strings.EqualFold(strings.TrimSpace(key), "Content-Length") {
					headers[i] = h[:strings.IndexByte(h, ':')+1] + " " + val
				}
			}
		} else if len(body) > 0 {
			headers = append(headers, "Content-Length: "+val)
		}
	}
	var sb strings.Builder
	sb.Grow(len(req) + 32)
	sb.WriteString(line)
	sb.WriteString("\r\n")
	for _, h := range headers {
		sb.WriteString(h)
		sb.WriteString("\r\n")
	}
	sb.WriteString("\r\n")
	sb.WriteString(body)
	return sb.String()
}

// applyPatterns - applies regex patterns to extract values from previous response
// Returns extracted values map and error if any pattern fails
// Note: patterns[relIdx-1] contains patterns for transition TO request relIdx
func applyPatterns(pats []pattern, prevResp string, logger LogWriter) (map[string]string, error) {
	values := make(map[string]string, len(pats))
	for _, p := range pats {
		m := p.re.FindStringSubmatch(prevResp)
		if m == nil {
			if verbose {
				logger.Write(fmt.Sprintf("[V] regex no match, pattern: %s\n", p.keyword))
			}
			return nil, fmt.Errorf("regex did not match for: %s", p.keyword)
		}

		extracted := strings.Clone(decodeExtracted(m[1]))
		values[p.keyword] = extracted
		logger.Write(fmt.Sprintf("Matched %s: %s\n", p.keyword, extracted))

		// persist _prefixed keys to global store
		if len(p.keyword) > 0 && p.keyword[0] == '_' {
			globalStaticVals.Store(p.keyword, extracted)
		}
	}

	return values, nil
}

// collectStaticValues - fills missing globals without replacing local overrides.
// Only collects for keys that exist in request and have values in global store
func collectStaticValues(keys []string, values map[string]string, logger LogWriter) {
	for _, key := range keys {
		if len(key) > 0 && key[0] == '_' {
			if v, exists := globalStaticVals.Load(key); exists {
				if _, exists := values[key]; !exists {
					values[key] = v.(string)
				}
				logger.Write(fmt.Sprintf("Static $%s$: %s\n", key, v.(string)))
			}
		}
	}
}
