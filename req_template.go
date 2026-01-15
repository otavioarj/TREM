package main

import (
	"fmt"
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
	Raw  string    // original request content
	Keys []KeyInfo // placeholders sorted by offset
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

// getUniqueKeyNames - returns deduplicated slice of key names
func getUniqueKeyNames(tmpl *TemplateReq) []string {
	if len(tmpl.Keys) == 0 {
		return nil
	}

	seen := make(map[string]bool, len(tmpl.Keys))
	names := make([]string, 0, len(tmpl.Keys))
	for _, k := range tmpl.Keys {
		if !seen[k.Name] {
			seen[k.Name] = true
			names = append(names, k.Name)
		}
	}
	return names
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

// normalizeReq - normalizes HTTP/1.1 request after all substitutions
// - ensures HTTP/1.1 in request line
// - normalizes line endings to \r\n
// - calculates and sets Content-Length
func normalizeReq(req string) string {
	reqLineEnd := strings.Index(req, "\n")
	if reqLineEnd < 0 {
		return req
	}
	requestLine := strings.TrimRight(req[:reqLineEnd], "\r")
	// ensure HTTP/1.1
	if !strings.Contains(requestLine, " HTTP/") {
		requestLine += " HTTP/1.1"
	} else if !strings.HasSuffix(requestLine, "HTTP/1.1") {
		requestLine = httpVersionRe.ReplaceAllString(requestLine, "HTTP/1.1")
	}
	rest := req[reqLineEnd+1:]
	// find headers/body separator
	var headersPart, body string
	if sepIdx := strings.Index(rest, "\r\n\r\n"); sepIdx >= 0 {
		headersPart = rest[:sepIdx]
		body = rest[sepIdx+4:]
	} else if sepIdx := strings.Index(rest, "\n\n"); sepIdx >= 0 {
		headersPart = rest[:sepIdx]
		body = rest[sepIdx+2:]
	} else {
		headersPart = strings.TrimRight(rest, "\r\n")
		body = ""
	}
	// process headers
	headerLines := strings.Split(headersPart, "\n")
	headers := make([]string, 0, len(headerLines))
	hasContentLen := false
	hasTransferEnc := false
	contentLenIdx := -1

	for _, line := range headerLines {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "content-length:") {
			hasContentLen = true
			contentLenIdx = len(headers)
		}
		if strings.HasPrefix(lower, "transfer-encoding:") {
			hasTransferEnc = true
		}
		headers = append(headers, line)
	}
	bodyLen := len(body)
	// set Content-Length (skip if Transfer-Encoding present)
	if !hasTransferEnc {
		if hasContentLen && contentLenIdx >= 0 {
			headers[contentLenIdx] = "Content-Length: " + strconv.Itoa(bodyLen)
		} else if bodyLen > 0 {
			headers = append(headers, "Content-Length: "+strconv.Itoa(bodyLen))
		}
	}
	// rebuild request
	var sb strings.Builder
	sb.Grow(len(req) + 32)
	sb.WriteString(requestLine)
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

		extracted := decodeExtracted(m[1])
		values[p.keyword] = extracted
		logger.Write(fmt.Sprintf("Matched %s: %s\n", p.keyword, extracted))

		// persist _prefixed keys to global store
		if len(p.keyword) > 0 && p.keyword[0] == '_' {
			globalStaticVals.Store(p.keyword, extracted)
		}
	}

	return values, nil
}

// collectStaticValues - collects values from globalStaticVals for keys in template
// Only collects for keys that exist in request and have values in global store
func collectStaticValues(keys []string, logger LogWriter) map[string]string {
	values := make(map[string]string)
	for _, key := range keys {
		if len(key) > 0 && key[0] == '_' {
			if v, exists := globalStaticVals.Load(key); exists {
				values[key] = v.(string)
				logger.Write(fmt.Sprintf("Static $%s$: %s\n", key, v.(string)))
			}
		}
	}

	return values
}
