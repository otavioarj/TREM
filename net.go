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

// send - creates new connection and sends request
func send(raw []byte, addr, proxyURL string, cliMode int, tlsTimeout time.Duration, logger LogWriter, threadID int) (resp, status string, err error) {
	conn, err := dialWithProxy(addr, proxyURL)
	if err != nil {
		return "", "", err
	}

	host, port, _ := net.SplitHostPort(addr)

	if port == "443" {
		tlsConn, err := tlsHandshake(conn, host, cliMode, tlsTimeout, logger, threadID)
		if err != nil {
			conn.Close()
			return "", "", err
		}
		conn = tlsConn
	}

	defer conn.Close()
	return sendOnConn(raw, conn, threadID)
}

// sendOnConn - sends request on existing connection, reports metrics
func sendOnConn(raw []byte, conn net.Conn, threadID int) (resp, status string, err error) {
	// Only measure latency in verbose mode
	var startTime time.Time
	if verbose {
		startTime = time.Now()
	}

	var sb strings.Builder
	var contentLength int64 = -1
	var chunked bool
	var encoding string
	var decodedBody bytes.Buffer

	bytesOut := len(raw)
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

	// Check for HTTP error
	statusCode, _ := strconv.Atoi(status)
	if statusCode >= 400 {
		stats.ReportHTTPError(threadID, statusCode)
	}

	// Parse headers
	var bytesIn int
	bytesIn += len(line)

	for {
		l, err := reader.ReadString('\n')
		if err != nil {
			return "", "", err
		}
		bytesIn += len(l)
		sb.WriteString(l)
		if l == "\r\n" || l == "\n" {
			break
		}

		lower := strings.ToLower(l)

		if contentLength < 0 {
			if strings.HasPrefix(lower, "content-length:") {
				val := strings.TrimSpace(l[15:])
				val = strings.TrimSuffix(val, "\r")
				contentLength, _ = strconv.ParseInt(val, 10, 64)
			}
		}

		if strings.HasPrefix(lower, "transfer-encoding:") {
			if strings.Contains(lower, "chunked") {
				chunked = true
			}
		}

		if encoding == "" {
			if strings.HasPrefix(lower, "content-encoding:") {
				encoding = strings.TrimSpace(strings.ToLower(l[17:]))
				encoding = strings.TrimSuffix(encoding, "\r")
			}
		}
	}

	// Read body
	body := &bytes.Buffer{}

	if chunked {
		for {
			sizeLine, err := reader.ReadString('\n')
			if err != nil {
				break
			}
			bytesIn += len(sizeLine)
			sizeLine = strings.TrimSpace(sizeLine)
			size, err := strconv.ParseInt(sizeLine, 16, 64)
			if err != nil || size == 0 {
				reader.ReadString('\n')
				break
			}

			chunk := make([]byte, size)
			n, err := io.ReadFull(reader, chunk)
			bytesIn += n
			if err != nil {
				break
			}
			body.Write(chunk)

			crlf, _ := reader.ReadString('\n')
			bytesIn += len(crlf)
		}
	} else if contentLength > 0 {
		limited := io.LimitReader(reader, contentLength)
		n, _ := io.Copy(body, limited)
		bytesIn += int(n)
	} else if contentLength == 0 {
		// No body
	} else {
		buf := make([]byte, 4096)
		for {
			n, err := reader.Read(buf)
			if n > 0 {
				bytesIn += n
				body.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
	}

	// Decode compressed body
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
		if verbose {
			fmt.Printf("[V] unknown encoding: %s, returning raw\n", encoding)
		}
		decodedBody = *body
	}

	sb.Write(decodedBody.Bytes())

	// Report metrics (latency only if verbose)
	var latencyUs uint32
	if verbose {
		latencyUs = uint32(time.Since(startTime).Microseconds())
	}
	stats.ReportRequest(threadID, latencyUs, uint32(bytesIn), uint32(bytesOut))

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

// dialWithRetry - dial with exponential backoff
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

		conn, err := dialWithProxy(addr, o.proxyURL)
		if err != nil {
			lastErr = err
			continue
		}

		host, port, _ := net.SplitHostPort(addr)
		if port == "443" {
			tlsConn, err := tlsHandshake(conn, host, o.clientHelloID, o.tlsTimeout, w.logger, w.id)
			if err != nil {
				conn.Close()
				lastErr = err
				continue
			}
			return tlsConn, nil
		}

		return conn, nil
	}

	return nil, fmt.Errorf("dial failed after %d retries: %v", o.maxRetries, lastErr)
}

// sendWithRetry - send with retry on error
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

		resp, status, err := send(raw, addr, o.proxyURL, o.clientHelloID, o.tlsTimeout, w.logger, w.id)
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

	resp, status, err := sendOnConn(raw, conn, w.id)
	if err != nil {
		if verbose {
			w.logger.Write(fmt.Sprintf("[V] send on conn failed: %v, reconnecting\n", err))
		}

		o.closeWorkerConn(w)

		newConn, dialErr := o.dialWithRetry(w, addr)
		if dialErr != nil {
			return "", "", fmt.Errorf("reconnect failed: %v (original: %v)", dialErr, err)
		}

		w.connMu.Lock()
		w.conn = newConn
		w.connAddr = addr
		w.connMu.Unlock()

		return sendOnConn(raw, newConn, w.id)
	}

	return resp, status, nil
}
