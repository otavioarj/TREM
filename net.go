package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	utls "github.com/refraction-networking/utls"
)

// send - creates new conn and sends request
func send(raw []byte, addr, proxyURL string, cliMode int, tlsTimeout time.Duration, tlsCert *utls.Certificate, logger LogWriter,
	threadID int, httpH2 bool) (resp, status string, err error) {
	conn, err := dialWithProxy(addr, proxyURL)
	if err != nil {
		return "", "", err
	}

	host, port, _ := net.SplitHostPort(addr)
	if port == "443" {
		tlsConn, err := tlsHandshakeDo(conn, host, cliMode, tlsTimeout, logger, threadID, tlsCert, httpH2)
		if err != nil {
			conn.Close()
			return "", "", err
		}
		conn = tlsConn
	}
	defer conn.Close()

	// Route by protocol - parse only when needed for H2
	if !httpH2 {
		return sendOnConn(raw, conn, threadID)
	}

	req := parseRawReq2H2(string(raw))
	if req == nil {
		return "", "", fmt.Errorf("failed to parse request")
	}
	h2 := newH2Conn(conn)
	return h2.sendReqH2(req, threadID, true)
}

// sendOnConn - sends request on existing connection, reports metrics
func sendOnConn(raw []byte, conn net.Conn, threadID int) (resp, status string, err error) {
	var startTime time.Time
	if verbose {
		startTime = time.Now()
	}

	bytesOut := len(raw)
	_, err = conn.Write(raw)
	if err != nil {
		return "", "", err
	}
	reader := bufio.NewReader(conn)
	resp, status, bytesIn, err := readHTTP1Resp(threadID, reader)
	if err != nil {
		return "", "", err
	}

	var latencyUs uint32
	if verbose {
		latencyUs = uint32(time.Since(startTime).Microseconds())
	}
	stats.ReportRequest(threadID, latencyUs, uint32(bytesIn), uint32(bytesOut))

	return resp, status, nil
}

// readHTTP1Resp - internal function that reads HTTP/1.1 response and returns bytesIn
func readHTTP1Resp(threadID int, reader *bufio.Reader) (resp, status string, bytesIn int, err error) {
	var sb strings.Builder
	var contentLength int64 = -1
	var chunked bool
	var encoding string

	line, err := reader.ReadString('\n')
	if err != nil {
		return "", "", 0, err
	}

	parts := strings.SplitN(line, " ", 3)
	if len(parts) >= 2 {
		status = parts[1]
	}

	statusCode, _ := strconv.Atoi(status)
	if statusCode >= 400 {
		stats.ReportHTTPError(threadID, statusCode)
	}
	sb.WriteString(line)
	bytesIn += len(line)

	for {
		l, err := reader.ReadString('\n')
		if err != nil {
			return "", "", bytesIn, err
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
	} else if contentLength == -1 { // header not found!
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

	decodedBytes, err := decodeBody(body, encoding)
	if err != nil {
		return "", "", bytesIn, err
	}
	sb.Write(decodedBytes)

	return sb.String(), status, bytesIn, nil
}

// setupSOCKS5Proxy - performs SOCKS5 handshake on an already-connected proxy conn
// After success, conn is ready to use as a tunnel to host:port
func setupSOCKS5Proxy(proxy net.Conn, host string, port int) error {
	// Handshake: version 5, 1 method, no auth
	if _, err := proxy.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		return err
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(proxy, resp); err != nil {
		return err
	}
	if resp[0] != 0x05 || resp[1] != 0x00 {
		return fmt.Errorf("SOCKS5 handshake failed")
	}

	// Build connect request
	req := []byte{0x05, 0x01, 0x00} // version, connect, reserved
	ip := net.ParseIP(host)
	if ip != nil && ip.To4() != nil {
		req = append(req, 0x01) // IPv4
		req = append(req, ip.To4()...)
	} else if ip != nil {
		req = append(req, 0x04) // IPv6
		req = append(req, ip.To16()...)
	} else {
		req = append(req, 0x03) // Domain
		req = append(req, byte(len(host)))
		req = append(req, []byte(host)...)
	}
	portBytes := make([]byte, 2)
	binary.BigEndian.PutUint16(portBytes, uint16(port))
	req = append(req, portBytes...)

	if _, err := proxy.Write(req); err != nil {
		return err
	}

	// Read response header
	resp = make([]byte, 4)
	if _, err := io.ReadFull(proxy, resp); err != nil {
		return err
	}
	if resp[1] != 0x00 {
		return fmt.Errorf("SOCKS5 connect failed: %d", resp[1])
	}

	// Skip bind address based on type
	switch resp[3] {
	case 0x01: // IPv4
		io.ReadFull(proxy, make([]byte, 4+2))
	case 0x03: // Domain
		var length byte
		binary.Read(proxy, binary.BigEndian, &length)
		io.ReadFull(proxy, make([]byte, int(length)+2))
	case 0x04: // IPv6
		io.ReadFull(proxy, make([]byte, 16+2))
	}

	if verbose {
		fmt.Printf("[V] SOCKS5 tunnel established to %s:%d\n", host, port)
	}
	return nil
}

// dialWithProxy - dials target through proxy (HTTP CONNECT or SOCKS5) or directly
func dialWithProxy(addr, proxyURL string) (net.Conn, error) {
	if proxyURL == "" {
		return net.DialTimeout("tcp", addr, 10*time.Second)
	}

	var isSocks5, isHTTP bool
	switch {
	case strings.HasPrefix(proxyURL, "socks5://") || strings.HasPrefix(proxyURL, "socks://"):
		isSocks5 = true
	case strings.HasPrefix(proxyURL, "http://"), strings.HasPrefix(proxyURL, "https://"):
		isHTTP = true
	default:
		return nil, fmt.Errorf("unsupported proxy scheme (use http(s)://  or socks(5)://): %s", proxyURL)
	}

	proxyAddr, err := parseProxyAddr(proxyURL)
	if err != nil {
		return nil, err
	}

	proxyConn, err := net.DialTimeout("tcp", proxyAddr, 5*time.Second)
	if err != nil {
		return nil, err
	}

	if isSocks5 {
		host, portStr, _ := net.SplitHostPort(addr)
		port, _ := strconv.Atoi(portStr)
		if err := setupSOCKS5Proxy(proxyConn, host, port); err != nil {
			proxyConn.Close()
			return nil, err
		}
		return proxyConn, nil
	}

	// isHTTP: HTTP CONNECT
	_ = isHTTP
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

// dialWithRetry - dial with retry, populates w.conn or w.h2conn based on o.httpH2
func (o *Orch) dialWithRetry(w *monkey, addr string) error {
	var lastErr error
	backoff := 100 * time.Millisecond
	h2 := o.httpH2

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
		// default is TLS!
		if port != "8080" || port != "80" {
			tlsConn, err := tlsHandshakeDo(conn, host, o.clientHelloID, o.tlsTimeout, w.logger, w.id, o.tlsCert, h2)
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
				w.logger.Write(fmt.Sprintf("[V] H2 HandShake err: %v\n", err))
				lastErr = err
				// Don't retry on H2 protocol errors - server doesn't support H2
				if errors.Is(err, errH2NotSupported) {
					return fmt.Errorf("H2 not supported: %v", err)
				}
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

// sendWithRetry - send with retry on error (routes by httpH2)
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

		resp, status, err := send(raw, addr, o.proxyURL, o.clientHelloID, o.tlsTimeout, o.tlsCert, w.logger, w.id, o.httpH2)
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
	if o.httpH2 {
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
	if o.httpH2 {
		return sendOnConnH2(raw, w.h2conn, w.id)
	}
	return sendOnConn(raw, w.conn, w.id)
}

// dialNewConn - creates new connection for fire-and-forget (non-keepalive mode)
func (o *Orch) dialNewConn(addr string) (net.Conn, error) {
	conn, err := dialWithProxy(addr, o.proxyURL)
	if err != nil {
		return nil, err
	}

	host, port, _ := net.SplitHostPort(addr)
	if port == "443" {
		tlsConn, err := tlsHandshakeDo(conn, host, o.clientHelloID, o.tlsTimeout, nil, -1, o.tlsCert, false)
		if err != nil {
			conn.Close()
			return nil, err
		}
		return tlsConn, nil
	}

	return conn, nil
}
