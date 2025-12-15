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

// sendH2 - creates new conn and sends request
func send(raw []byte, addr, proxyURL string, cliMode int, tlsTimeout time.Duration, logger LogWriter, threadID int, httpVer int) (resp, status string, err error) {
	conn, err := dialWithProxy(addr, proxyURL)
	if err != nil {
		return "", "", err
	}

	host, port, _ := net.SplitHostPort(addr)
	if port == "443" {
		tlsConn, err := tlsHandshakeDo(conn, host, cliMode, tlsTimeout, logger, threadID, true)
		if err != nil {
			conn.Close()
			return "", "", err
		}
		conn = tlsConn
	}
	defer conn.Close()

	req := parseRawReq2H2(string(raw))
	if req == nil {
		return "", "", fmt.Errorf("failed to parse request")
	}

	if httpVer == 1 {
		return sendOnConn(raw, conn, threadID)
	} else {
		h2 := newH2Conn(conn)
		return h2.sendReqH2(req, threadID)
	}
}

// sendOnConn - sends request on existing connection, reports metrics
func sendOnConn(raw []byte, conn net.Conn, threadID int) (resp, status string, err error) {
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

	statusCode, _ := strconv.Atoi(status)
	if statusCode >= 400 {
		stats.ReportHTTPError(threadID, statusCode)
	}

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

// closeWorkerConn - safely close worker connection (HTTP/1 and HTTP/2)
func (o *Orch) closeWorkerConn(w *monkey) {
	w.connMu.Lock()
	defer w.connMu.Unlock()
	if w.conn != nil {
		w.conn.Close()
		w.conn = nil
	}
	if w.h2conn != nil {
		w.h2conn.conn.Close()
		w.h2conn = nil
	}
}

// dialWithRetry - dial with retry, populates w.conn or w.h2conn based on o.httpVer
func (o *Orch) dialWithRetry(w *monkey, addr string) error {
	var lastErr error
	backoff := 100 * time.Millisecond
	h2 := o.httpVer == 2

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
			tlsConn, err := tlsHandshakeDo(conn, host, o.clientHelloID, o.tlsTimeout, w.logger, w.id, h2)
			if err != nil {
				conn.Close()
				lastErr = err
				continue
			}
			conn = tlsConn
		}

		// HTTP/2: wrap in H2Conn and handshake
		if h2 {
			h2c := newH2Conn(conn)
			if err := h2c.handshake(); err != nil {
				conn.Close()
				lastErr = err
				continue
			}
			w.connMu.Lock()
			w.h2conn = h2c
			w.connAddr = addr
			w.connMu.Unlock()
			return nil
		}

		// HTTP/1.1
		w.connMu.Lock()
		w.conn = conn
		w.connAddr = addr
		w.connMu.Unlock()
		return nil
	}

	return fmt.Errorf("dial failed after %d retries: %v", o.maxRetries, lastErr)
}

// sendWithRetry - send with retry on error (routes by httpVer)
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

		resp, status, err := send(raw, addr, o.proxyURL, o.clientHelloID, o.tlsTimeout, w.logger, w.id, o.httpVer)
		if err == nil {
			return resp, status, nil
		}
		lastErr = err
	}

	return "", "", fmt.Errorf("send failed after %d retries: %v", o.maxRetries, lastErr)
}

// sendWithReconnect - send on existing conn, reconnect on error
func (o *Orch) sendWithReconnect(w *monkey, raw []byte, addr string) (string, string, error) {
	var resp, status string
	var err error

	// Try existing connection
	if o.httpVer == 2 {
		w.connMu.Lock()
		h2 := w.h2conn
		w.connMu.Unlock()

		if h2 != nil {
			resp, status, err = sendOnConnH2(raw, h2, w.id)
			if err == nil {
				return resp, status, nil
			}
			if verbose {
				w.logger.Write(fmt.Sprintf("[V] h2 send failed: %v, reconnecting\n", err))
			}
		}
	} else {
		w.connMu.Lock()
		conn := w.conn
		w.connMu.Unlock()

		if conn != nil {
			resp, status, err = sendOnConn(raw, conn, w.id)
			if err == nil {
				return resp, status, nil
			}
			if verbose {
				w.logger.Write(fmt.Sprintf("[V] send failed: %v, reconnecting\n", err))
			}
		}
	}

	// Reconnect
	o.closeWorkerConn(w)
	if dialErr := o.dialWithRetry(w, addr); dialErr != nil {
		return "", "", fmt.Errorf("reconnect failed: %v (original: %v)", dialErr, err)
	}

	// Retry on new connection
	if o.httpVer == 2 {
		return sendOnConnH2(raw, w.h2conn, w.id)
	}
	return sendOnConn(raw, w.conn, w.id)
}
