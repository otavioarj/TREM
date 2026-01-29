package main

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"

	utls "github.com/refraction-networking/utls"
)

// Pre-compiled regex for parseHost
// The (?m) flag enables multiline mode where ^ matches at line start
var hostHeaderRe = regexp.MustCompile(`(?im)^Host:\s*([^:\r\n]+)(?::(\d+))?`)

// httpVersionRe - matches HTTP/x.x at end of request line
var httpVersionRe = regexp.MustCompile(`HTTP/\d\.\d`)

// decodeBody - decompresses gzip/deflate encoded body
// Returns raw bytes if encoding is empty, identity, or unknown
func decodeBody(body *bytes.Buffer, encoding string) ([]byte, error) {
	switch encoding {
	case "", "identity":
		return body.Bytes(), nil

	case "gzip":
		r, err := gzip.NewReader(body)
		if err != nil {
			return nil, fmt.Errorf("gzip open: %v", err)
		}
		defer r.Close()
		decoded, err := io.ReadAll(r)
		if err != nil {
			return nil, fmt.Errorf("gzip read: %v", err)
		}
		return decoded, nil

	case "deflate":
		r := flate.NewReader(body)
		defer r.Close()
		decoded, err := io.ReadAll(r)
		if err != nil {
			return nil, fmt.Errorf("deflate read: %v", err)
		}
		return decoded, nil

	default:
		if verbose {
			fmt.Printf("[V] unknown encoding: %s, returning raw\n", encoding)
		}
		return body.Bytes(), nil
	}
}

// parseHost - extracts host:port from request Host header or override
// Uses regex with (?m) multiline flag to match directly without splitting lines
// This is more efficient than iterating over all lines
func parseHost(req, override string) (string, error) {
	if override != "" {
		return override, nil
	}

	// hostHeaderRe has (?m) flag, so ^ matches line start in multiline string
	if m := hostHeaderRe.FindStringSubmatch(req); m != nil {
		h := m[1]
		port := m[2]
		if port == "" {
			return h + ":443", nil
		}
		return h + ":" + port, nil
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

// hexVal - converts hex char to value
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

// loadMTLSCert - loads mTLS certificate if specified
func loadMTLSCert(mtlsFlag string) *utls.Certificate {
	if mtlsFlag == "" {
		return nil
	}
	certAndPass := strings.Split(mtlsFlag, ":")
	if len(certAndPass) != 2 {
		exitErr("Client Certificate (mTLS) must be file:pass!")
	}
	cert, err := loadPKCS12Certificate(certAndPass[0], certAndPass[1])
	if err != nil {
		exitErr(err.Error())
	}
	return &cert
}
