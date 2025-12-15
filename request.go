package main

import (
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
//	TREM always get request as HTTP/1.1, so pseudoheader
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
