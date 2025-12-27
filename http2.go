package main

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/http2/hpack"
)

// HTTP/2 frame types
const (
	frameData         = 0x0
	frameHeaders      = 0x1
	framePriority     = 0x2
	frameRstStream    = 0x3
	frameSettings     = 0x4
	framePushPromise  = 0x5
	framePing         = 0x6
	frameGoAway       = 0x7
	frameWindowUpdate = 0x8
	frameContinuation = 0x9
)

// HTTP/2 flags
const (
	flagEndStream  = 0x1
	flagEndHeaders = 0x4
	flagPadded     = 0x8
	flagPriority   = 0x20
)

// HTTP/2 connection preface
var h2Preface = []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")

// Pre-built empty SETTINGS frame (no custom settings)
var h2SettingsFrame = []byte{0, 0, 0, frameSettings, 0, 0, 0, 0, 0}

// H2Conn - wraps net.Conn with HTTP/2 state
type H2Conn struct {
	conn      net.Conn
	enc       *hpack.Encoder
	dec       *hpack.Decoder
	encBuf    bytes.Buffer
	streamID  uint32
	handshook bool
}

// newH2Conn - wraps conn for HTTP/2
func newH2Conn(conn net.Conn) *H2Conn {
	h := &H2Conn{
		conn:     conn,
		streamID: 1, // client streams are odd
	}
	h.enc = hpack.NewEncoder(&h.encBuf)
	h.dec = hpack.NewDecoder(4096, nil)
	return h
}

// handshake - sends connection preface and settings
func (h *H2Conn) handshake() error {
	if h.handshook {
		return nil
	}

	// Send preface
	if _, err := h.conn.Write(h2Preface); err != nil {
		return err
	}

	// Send empty SETTINGS
	if _, err := h.conn.Write(h2SettingsFrame); err != nil {
		return err
	}

	// Read server SETTINGS (just consume, don't process)
	if err := h.readAndAckSettings(); err != nil {
		return err
	}

	h.handshook = true
	return nil
}

// readAndAckSettings - reads server SETTINGS and sends ACK
func (h *H2Conn) readAndAckSettings() error {
	hdr := make([]byte, 9)
	if _, err := io.ReadFull(h.conn, hdr); err != nil {
		return err
	}

	length := int(hdr[0])<<16 | int(hdr[1])<<8 | int(hdr[2])
	ftype := hdr[3]

	// Read payload if any
	if length > 0 {
		payload := make([]byte, length)
		if _, err := io.ReadFull(h.conn, payload); err != nil {
			return err
		}
	}

	// If it's SETTINGS (not ACK), send ACK
	if ftype == frameSettings && hdr[4]&0x1 == 0 {
		ack := []byte{0, 0, 0, frameSettings, 0x1, 0, 0, 0, 0}
		if _, err := h.conn.Write(ack); err != nil {
			return err
		}
	}

	return nil
}

// sendReqH2 - sends HTTP/2 request, returns response
func (h *H2Conn) sendReqH2(req *ParsedReq, threadID int) (resp, status string, err error) {
	var startTime time.Time
	if verbose {
		startTime = time.Now()
	}

	if err := h.handshake(); err != nil {
		return "", "", err
	}

	streamID := h.streamID
	h.streamID += 2 // next odd number

	// Encode headers
	h.encBuf.Reset()
	h.enc.WriteField(hpack.HeaderField{Name: ":method", Value: req.Method})
	h.enc.WriteField(hpack.HeaderField{Name: ":path", Value: req.Path})
	h.enc.WriteField(hpack.HeaderField{Name: ":scheme", Value: "https"})
	h.enc.WriteField(hpack.HeaderField{Name: ":authority", Value: req.Host})

	for _, hdr := range req.Headers {
		// Lowercase header names for h2
		h.enc.WriteField(hpack.HeaderField{
			Name:  strings.ToLower(hdr[0]),
			Value: hdr[1],
		})
	}

	headerBlock := h.encBuf.Bytes()
	hasBody := len(req.Body) > 0

	// Build HEADERS frame
	flags := byte(flagEndHeaders)
	if !hasBody {
		flags |= flagEndStream
	}

	bytesOut := 9 + len(headerBlock)
	frame := buildFrame(frameHeaders, flags, streamID, headerBlock)
	if _, err := h.conn.Write(frame); err != nil {
		return "", "", err
	}

	// Send DATA frame if body exists
	if hasBody {
		// NOTE: Flow control ignored - assumes server window >= 64KB (default)
		// Bodies larger than 64KB may stall without WINDOW_UPDATE handling
		dataFrame := buildFrame(frameData, flagEndStream, streamID, req.Body)
		bytesOut += 9 + len(req.Body)
		if _, err := h.conn.Write(dataFrame); err != nil {
			return "", "", err
		}
	}

	// Read response
	resp, status, bytesIn, err := h.readResponse(streamID)
	if err != nil {
		return "", "", err
	}

	// Report stats
	statusCode, _ := strconv.Atoi(status)
	if statusCode >= 400 {
		stats.ReportHTTPError(threadID, statusCode)
	}

	var latencyUs uint32
	if verbose {
		latencyUs = uint32(time.Since(startTime).Microseconds())
	}
	stats.ReportRequest(threadID, latencyUs, uint32(bytesIn), uint32(bytesOut))
	return resp, status, nil
}

// readResponse - reads frames until END_STREAM
func (h *H2Conn) readResponse(streamID uint32) (resp, status string, bytesIn int, err error) {
	var headers [][2]string
	var body bytes.Buffer
	var encoding string

	// Reuse header buffer across frame reads
	hdr := make([]byte, 9)

	for {
		n, err := io.ReadFull(h.conn, hdr)
		bytesIn += n
		if err != nil {
			return "", "", bytesIn, err
		}

		// H2 uses size in BIGEndian, lets bitwise it :)
		length := int(hdr[0])<<16 | int(hdr[1])<<8 | int(hdr[2])
		ftype := hdr[3]
		flags := hdr[4]
		// 4 bytes into correct endian
		fStreamID := uint32(hdr[5]&0x7F)<<24 | uint32(hdr[6])<<16 | uint32(hdr[7])<<8 | uint32(hdr[8])

		var payload []byte
		if length > 0 {
			payload = make([]byte, length)
			n, err := io.ReadFull(h.conn, payload)
			bytesIn += n
			if err != nil {
				return "", "", bytesIn, err
			}
		}

		// NOTE: Server push (PUSH_PROMISE) ignored - frames discarded
		// Most APIs don't use push; browsers are primary consumers

		switch ftype {
		case frameHeaders, frameContinuation:
			if fStreamID != streamID {
				continue // different stream, skip
			}

			// Decode HPACK
			hdrs, err := h.dec.DecodeFull(payload)
			if err != nil {
				return "", "", bytesIn, fmt.Errorf("hpack decode: %v", err)
			}

			for _, hf := range hdrs {
				if hf.Name == ":status" {
					status = hf.Value
				} else {
					headers = append(headers, [2]string{hf.Name, hf.Value})
					if hf.Name == "content-encoding" {
						encoding = hf.Value
					}
				}
			}

		case frameData:
			if fStreamID != streamID {
				continue
			}
			// Handle padding if present
			if flags&flagPadded != 0 && len(payload) > 0 {
				padLen := int(payload[0])
				if padLen < len(payload)-1 {
					payload = payload[1 : len(payload)-padLen]
				}
			}
			body.Write(payload)

		case frameSettings:
			// ACK if not already ACK
			if flags&0x1 == 0 {
				ack := []byte{0, 0, 0, frameSettings, 0x1, 0, 0, 0, 0}
				h.conn.Write(ack)
			}

		case frameWindowUpdate:
			// Ignored - we don't track send window
			continue

		case framePing:
			// Respond to PING
			if flags&0x1 == 0 { // not ACK
				pong := buildFrame(framePing, 0x1, 0, payload)
				h.conn.Write(pong)
			}

		case frameGoAway:
			return "", "", bytesIn, fmt.Errorf("GOAWAY received")

		case frameRstStream:
			if fStreamID == streamID {
				// 4 bytes into correct endian
				errCode := uint32(payload[0])<<24 | uint32(payload[1])<<16 | uint32(payload[2])<<8 | uint32(payload[3])
				return "", "", bytesIn, fmt.Errorf("RST_STREAM: %d", errCode)
			}

		case framePushPromise:
			// Server push ignored - just consume and discard
			continue
		}

		// Check END_STREAM on this stream
		if fStreamID == streamID && (flags&flagEndStream) != 0 {
			break
		}
	}

	// Build response string (headers + body)
	var sb strings.Builder
	for _, hdr := range headers {
		sb.WriteString(hdr[0])
		sb.WriteString(": ")
		sb.WriteString(hdr[1])
		sb.WriteString("\r\n")
	}
	sb.WriteString("\r\n")

	// Decode body if compressed
	decodedBody := decodeBody(&body, encoding)
	sb.Write(decodedBody)

	return sb.String(), status, bytesIn, nil
}

// buildFrame - builds HTTP/2 frame
func buildFrame(ftype, flags byte, streamID uint32, payload []byte) []byte {
	length := len(payload)
	frame := make([]byte, 9+length)
	frame[0] = byte(length >> 16)
	frame[1] = byte(length >> 8)
	frame[2] = byte(length)
	frame[3] = ftype
	frame[4] = flags
	binary.BigEndian.PutUint32(frame[5:], streamID)
	copy(frame[9:], payload)
	return frame
}

// decodeBody - decompresses body if needed
func decodeBody(body *bytes.Buffer, encoding string) []byte {
	switch encoding {
	case "gzip":
		r, err := gzip.NewReader(body)
		if err != nil {
			return body.Bytes()
		}
		defer r.Close()
		decoded, _ := io.ReadAll(r)
		return decoded
	case "deflate":
		r := flate.NewReader(body)
		defer r.Close()
		decoded, _ := io.ReadAll(r)
		return decoded
	default:
		return body.Bytes()
	}
}

// sendOnConnH2 - sends on existing H2 connection
func sendOnConnH2(raw []byte, h2 *H2Conn, threadID int) (resp, status string, err error) {
	req := parseRawReq2H2(string(raw))
	if req == nil {
		return "", "", fmt.Errorf("failed to parse request")
	}
	return h2.sendReqH2(req, threadID)
}
