package main

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// send - creates new connection and sends req
func send(raw []byte, addr, proxyURL string, cliMode int, tlsTimeout time.Duration, logger LogWriter) (resp, status string, err error) {
	conn, err := dialWithProxy(addr, proxyURL)
	if err != nil {
		return "", "", err
	}

	host, port, _ := net.SplitHostPort(addr)

	if port == "443" {
		tlsConn, err := tlsHandshake(conn, host, cliMode, tlsTimeout, logger)
		if err != nil {
			conn.Close()
			return "", "", err
		}
		conn = tlsConn
	}

	defer conn.Close()
	return sendOnConn(raw, conn)
}

// sendOnConn - sends req on existing connection
func sendOnConn(raw []byte, conn net.Conn) (resp, status string, err error) {
	var sb strings.Builder
	var contentLength int64 = -1
	var chunked bool
	var encoding string
	var decodedBody bytes.Buffer

	_, err = conn.Write(raw)
	if err != nil {
		return "", "", err
	}

	reader := bufio.NewReader(conn)
	line, err := reader.ReadString('\n')
	if err != nil {
		return "", "", err
	}

	parts := strings.SplitN(line, " ", 3)
	if len(parts) >= 2 {
		status = parts[1]
	}

	// Parse headers
	for {
		l, err := reader.ReadString('\n')
		if err != nil {
			return "", "", err
		}
		sb.WriteString(l)
		if l == "\r\n" || l == "\n" {
			break
		}

		lower := strings.ToLower(l)

		// Parse Content-Length
		if contentLength < 0 {
			if strings.HasPrefix(lower, "content-length:") {
				val := strings.TrimSpace(l[15:])
				val = strings.TrimSuffix(val, "\r")
				contentLength, _ = strconv.ParseInt(val, 10, 64)
			}
		}

		// Parse Transfer-Encoding
		if strings.HasPrefix(lower, "transfer-encoding:") {
			if strings.Contains(lower, "chunked") {
				chunked = true
			}
		}

		// Parse Content-Encoding
		if encoding == "" {
			if strings.HasPrefix(lower, "content-encoding:") {
				encoding = strings.TrimSpace(strings.ToLower(l[17:]))
				encoding = strings.TrimSuffix(encoding, "\r")
			}
		}
	}

	// Read body based on transfer type
	body := &bytes.Buffer{}

	if chunked {
		// Handle chunked transfer encoding
		for {
			sizeLine, err := reader.ReadString('\n')
			if err != nil {
				break
			}
			sizeLine = strings.TrimSpace(sizeLine)
			size, err := strconv.ParseInt(sizeLine, 16, 64)
			if err != nil || size == 0 {
				// Read trailing CRLF after last chunk
				reader.ReadString('\n')
				break
			}

			chunk := make([]byte, size)
			_, err = io.ReadFull(reader, chunk)
			if err != nil {
				break
			}
			body.Write(chunk)

			// Read CRLF after chunk
			reader.ReadString('\n')
		}
	} else if contentLength > 0 {
		// Known content length
		limited := io.LimitReader(reader, contentLength)
		io.Copy(body, limited)
	} else if contentLength == 0 {
		// No body
	} else {
		// Unknown length, read until EOF
		buf := make([]byte, 4096)
		for {
			n, err := reader.Read(buf)
			if n > 0 {
				body.Write(buf[:n])
			}
			if err != nil {
				break // EOF or error, both mean end of body
			}
		}
	}

	// Decode body if compressed
	switch encoding {
	case "", "identity":
		decodedBody = *body
	case "gzip":
		bodyPlain, err := gzip.NewReader(body)
		if err != nil {
			return "", "", fmt.Errorf("gzip open err: %v", err)
		}
		defer bodyPlain.Close()
		_, err = io.Copy(&decodedBody, bodyPlain)
		if err != nil {
			return "", "", fmt.Errorf("gzip read err: %v", err)
		}
	case "deflate":
		bodyPlain := flate.NewReader(body)
		defer bodyPlain.Close()
		_, err = io.Copy(&decodedBody, bodyPlain)
		if err != nil {
			return "", "", fmt.Errorf("deflate read err: %v", err)
		}
	default:
		// Unknown encoding, return raw body
		if verbose {
			fmt.Printf("[V] unknown encoding: %s, returning raw\n", encoding)
		}
		decodedBody = *body
	}

	sb.Write(decodedBody.Bytes())
	return sb.String(), status, nil
}

func dialWithProxy(addr, proxyURL string) (net.Conn, error) {
	if proxyURL == "" {
		return net.DialTimeout("tcp", addr, 10*time.Second)
	}

	proxy, err := url.Parse(proxyURL)
	if err != nil {
		return nil, err
	}

	proxyConn, err := net.DialTimeout("tcp", proxy.Host, 10*time.Second)
	if err != nil {
		return nil, err
	}

	connectReq := fmt.Sprintf("CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", addr, addr)
	_, err = proxyConn.Write([]byte(connectReq))
	if err != nil {
		proxyConn.Close()
		return nil, err
	}

	// Use buffered reader instead of byte-by-byte
	reader := bufio.NewReader(proxyConn)
	respLine, err := reader.ReadString('\n')
	if err != nil {
		proxyConn.Close()
		return nil, err
	}

	if !strings.Contains(respLine, "200") {
		proxyConn.Close()
		return nil, fmt.Errorf("proxy CONNECT failed: %s", strings.TrimSpace(respLine))
	}

	// Read remaining headers until empty line
	for {
		line, err := reader.ReadString('\n')
		if err != nil || line == "\r\n" || line == "\n" {
			break
		}
	}

	return proxyConn, nil
}

// closeWorkerConn - safely close worker connection
func (o *Orch) closeWorkerConn(w *monkey) {
	w.connMu.Lock()
	defer w.connMu.Unlock()
	if w.conn != nil {
		w.conn.Close()
		w.conn = nil
	}
}

// dialWithRetry - dial with exponential backoff retry
func (o *Orch) dialWithRetry(w *monkey, addr string) (net.Conn, error) {
	var lastErr error
	backoff := 100 * time.Millisecond

	for attempt := 0; attempt < o.maxRetries; attempt++ {
		if attempt > 0 {
			if verbose {
				w.logger.Write(fmt.Sprintf("[V] dial retry %d/%d, backoff %v, err: %v\n",
					attempt+1, o.maxRetries, backoff, lastErr))
			}
			time.Sleep(backoff)
			backoff *= 2
			if backoff > 2*time.Second {
				backoff = 2 * time.Second
			}
		}

		// New TCP connection for each attempt
		conn, err := dialWithProxy(addr, o.proxyURL)
		if err != nil {
			lastErr = err
			continue
		}

		host, port, _ := net.SplitHostPort(addr)
		if port == "443" {
			// New TLS handshake on fresh connection!
			tlsConn, err := tlsHandshake(conn, host, o.clientHelloID, o.tlsTimeout, w.logger)
			if err != nil {
				conn.Close() // Close TCP conn on TLS failure
				lastErr = err
				continue
			}
			return tlsConn, nil
		}

		return conn, nil
	}

	return nil, fmt.Errorf("dial failed after %d retries: %v", o.maxRetries, lastErr)
}

// sendWithRetry - send with retry on any error
func (o *Orch) sendWithRetry(w *monkey, raw []byte, addr string) (string, string, error) {
	var lastErr error
	backoff := 100 * time.Millisecond

	for attempt := 0; attempt < o.maxRetries; attempt++ {
		if attempt > 0 {
			if verbose {
				w.logger.Write(fmt.Sprintf("[V] send retry %d/%d, err: %v\n",
					attempt+1, o.maxRetries, lastErr))
			}
			time.Sleep(backoff)
			backoff *= 2
			if backoff > 2*time.Second {
				backoff = 2 * time.Second
			}
		}

		resp, status, err := send(raw, addr, o.proxyURL, o.clientHelloID, o.tlsTimeout, w.logger)
		if err == nil {
			return resp, status, nil
		}
		lastErr = err
	}

	return "", "", fmt.Errorf("send failed after %d retries: %v", o.maxRetries, lastErr)
}

// sendWithReconnect - send on existing conn, reconnect on error
func (o *Orch) sendWithReconnect(w *monkey, raw []byte, conn net.Conn, addr string) (string, string, error) {
	if conn == nil {
		return o.sendWithRetry(w, raw, addr)
	}

	resp, status, err := sendOnConn(raw, conn)
	if err != nil {
		if verbose {
			w.logger.Write(fmt.Sprintf("[V] send on conn failed: %v, reconnecting\n", err))
		}

		// Close old connection
		o.closeWorkerConn(w)

		// Establish new connection
		newConn, dialErr := o.dialWithRetry(w, addr)
		if dialErr != nil {
			return "", "", fmt.Errorf("reconnect failed: %v (original: %v)", dialErr, err)
		}

		// Update worker connection
		w.connMu.Lock()
		w.conn = newConn
		w.connAddr = addr
		w.connMu.Unlock()

		// Retry send
		return sendOnConn(raw, newConn)
	}

	return resp, status, nil
}
