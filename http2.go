package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/http2/hpack"
)

// HTTP/2 protocol error - should not retry
var errH2NotSupported = errors.New("server does not support HTTP/2")

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

// HTTP/2 max frame size (default, can be negotiated higher)
const maxFrameSize = 16384

// HTTP/2 settings identifiers
const (
	settingsHeaderTableSize      = 0x1
	settingsEnablePush           = 0x2
	settingsMaxConcurrentStreams = 0x3
	settingsInitialWindowSize    = 0x4
	settingsMaxFrameSize         = 0x5
	settingsMaxHeaderListSize    = 0x6
)

// HTTP/2 connection preface
var h2Preface = []byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n")

// Pre-built SETTINGS frame with client parameters, expected by some H2 servers
// Settings: HEADER_TABLE_SIZE=4096, ENABLE_PUSH=0, MAX_CONCURRENT_STREAMS=100,

// INITIAL_WINDOW_SIZE=65535, MAX_FRAME_SIZE=16384
var h2SettingsFrame = []byte{
	0, 0, 30, // length: 30 bytes (5 settings * 6 bytes each)
	frameSettings, 0, // type=SETTINGS, flags=0
	0, 0, 0, 0, // stream ID = 0
	// SETTINGS_HEADER_TABLE_SIZE = 4096
	0, settingsHeaderTableSize, 0, 0, 0x10, 0x00,
	// SETTINGS_ENABLE_PUSH = 0
	0, settingsEnablePush, 0, 0, 0, 0,
	// SETTINGS_MAX_CONCURRENT_STREAMS = 100
	0, settingsMaxConcurrentStreams, 0, 0, 0, 100,
	// SETTINGS_INITIAL_WINDOW_SIZE = 65535
	0, settingsInitialWindowSize, 0, 0, 0xFF, 0xFF,
	// SETTINGS_MAX_FRAME_SIZE = 16384
	0, settingsMaxFrameSize, 0, 0, 0x40, 0x00,
}

// Pre-built WINDOW_UPDATE frame for connection (stream 0)
// Increment: 15MB (15728640 = 0x00F00000) - gives server plenty of send window
var h2WindowUpdate = []byte{
	0, 0, 4, // length: 4 bytes
	frameWindowUpdate, 0, // type=WINDOW_UPDATE, flags=0
	0, 0, 0, 0, // stream ID = 0 (connection-level)
	0x00, 0xF0, 0x00, 0x00, // increment = 15728640
}

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

	// Send preface + SETTINGS + WINDOW_UPDATE in single write
	// This is more efficient
	buf := make([]byte, len(h2Preface)+len(h2SettingsFrame)) //+len(h2WindowUpdate))
	n := copy(buf, h2Preface)
	n += copy(buf[n:], h2SettingsFrame)
	//copy(buf[n:], h2WindowUpdate)

	if k, err := h.conn.Write(buf); err != nil || k != n {
		return fmt.Errorf("%w: cannot write H2 tunnel! Buffer:%d Wrote:%d", err, n, k)
	}

	// Read server SETTINGS and send ACK
	if err := h.readAndAckSettings(); err != nil {
		return err
	}

	h.handshook = true
	return nil
}

// readAndAckSettings - reads frames until server SETTINGS, then sends ACK
// Returns errH2NotSupported if server sends invalid H2 data (e.g. HTTP/1.1 response)
func (h *H2Conn) readAndAckSettings() error {
	buf := make([]byte, 9)

	// Set read deadline for handshake (short timeout)
	h.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer h.conn.SetReadDeadline(time.Time{})

	// Read first frame header with validation
	n, err := h.conn.Read(buf)
	if err != nil {
		return fmt.Errorf("%w: cannot read H2 tunnel! B:%d", err, n)
	}

	// Need at least 5 bytes to detect HTTP/1.1 or validate H2 frame
	if n < 5 {
		return fmt.Errorf("%w: server responded invalid data!", errH2NotSupported)
	} else if bytes.HasPrefix(buf[:n], []byte("HTTP/")) {
		return fmt.Errorf("%w: server supports ONLY HTTP/1.1", errH2NotSupported)
	}

	// Validate first frame header
	if err := validateH2FrameHeader(buf[:n]); err != nil {
		return err
	}

	// Complete reading if we don't have full 9 bytes yet
	if n < 9 {
		if _, err := io.ReadFull(h.conn, buf[n:]); err != nil {
			return err
		}
	}

	// Process first frame and continue loop
	hdr := buf
	for {
		length := int(hdr[0])<<16 | int(hdr[1])<<8 | int(hdr[2])
		ftype := hdr[3]
		flags := hdr[4]

		// Read payload if any
		var payload []byte
		if length > 0 {
			payload = make([]byte, length)
			if _, err := io.ReadFull(h.conn, payload); err != nil {
				return err
			}
		}

		switch ftype {
		case frameSettings:
			// If it's SETTINGS (not ACK), send ACK
			if flags&0x1 == 0 {
				ack := []byte{0, 0, 0, frameSettings, 0x1, 0, 0, 0, 0}
				if _, err := h.conn.Write(ack); err != nil {
					return err
				}
			}
			return nil // Got SETTINGS, handshake complete

		case frameWindowUpdate:
			// Consume and continue waiting for SETTINGS
			break

		case framePing:
			// Respond to PING if not ACK
			if flags&0x1 == 0 {
				pong := buildFrame(framePing, 0x1, 0, payload)
				h.conn.Write(pong)
			}
			break

		case frameGoAway:
			errCode := uint32(0)
			if len(payload) >= 8 {
				errCode = uint32(payload[4])<<24 | uint32(payload[5])<<16 | uint32(payload[6])<<8 | uint32(payload[7])
			}
			return fmt.Errorf("GOAWAY during handshake, error code: %d", errCode)
		}

		// Read next frame header
		if _, err := io.ReadFull(h.conn, hdr); err != nil {
			return err
		}

		// Validate subsequent frames too
		if err := validateH2FrameHeader(hdr); err != nil {
			return err
		}
	}
}

// validateH2FrameHeader - validates H2 frame header bytes
// Returns errH2NotSupported for invalid data
func validateH2FrameHeader(hdr []byte) error {
	if len(hdr) < 5 {
		return fmt.Errorf("%w: insufficient header bytes", errH2NotSupported)
	}

	length := int(hdr[0])<<16 | int(hdr[1])<<8 | int(hdr[2])
	ftype := hdr[3]

	// Validate frame length
	if length > maxFrameSize*2 { // Allow some margin
		return fmt.Errorf("%w: invalid frame length %d", errH2NotSupported, length)
	}

	// Validate frame type (0x0 - 0x9 are valid)
	if ftype > frameContinuation {
		return fmt.Errorf("%w: invalid frame type 0x%02x", errH2NotSupported, ftype)
	}

	// For SETTINGS frame, streamID must be 0
	if ftype == frameSettings && len(hdr) >= 9 {
		streamID := uint32(hdr[5]&0x7F)<<24 | uint32(hdr[6])<<16 | uint32(hdr[7])<<8 | uint32(hdr[8])
		if streamID != 0 {
			return fmt.Errorf("%w: SETTINGS frame with non-zero stream ID", errH2NotSupported)
		}
	}

	return nil
}

// encodeReqFrames - encodes request into H2 frames (HEADERS + optional DATA)
// Writes directly to dst buffer for zero-copy batching
func (h *H2Conn) encodeReqFrames(dst *bytes.Buffer, req *ParsedReq, streamID uint32) {
	h.encBuf.Reset()
	h.enc.WriteField(hpack.HeaderField{Name: ":method", Value: req.Method})
	h.enc.WriteField(hpack.HeaderField{Name: ":path", Value: req.Path})
	h.enc.WriteField(hpack.HeaderField{Name: ":scheme", Value: "https"})
	h.enc.WriteField(hpack.HeaderField{Name: ":authority", Value: req.Host})

	for _, hdr := range req.Headers {
		h.enc.WriteField(hpack.HeaderField{
			Name:  strings.ToLower(hdr[0]),
			Value: hdr[1],
		})
	}

	headerBlock := h.encBuf.Bytes()
	hasBody := len(req.Body) > 0

	flags := byte(flagEndHeaders)
	if !hasBody {
		flags |= flagEndStream
	}

	dst.Write(buildFrame(frameHeaders, flags, streamID, headerBlock))

	if hasBody {
		dst.Write(buildFrame(frameData, flagEndStream, streamID, req.Body))
	}
}

// sendReqH2 - sends HTTP/2 request, returns response
func (h *H2Conn) sendReqH2(req *ParsedReq, threadID int, waitRsp bool) (resp, status string, err error) {
	var startTime time.Time
	if verbose {
		startTime = time.Now()
	}

	if err := h.handshake(); err != nil {
		return "", "", err
	}

	streamID := h.streamID
	h.streamID += 2 // next odd number

	// Encode and send frames
	var buf bytes.Buffer
	h.encodeReqFrames(&buf, req, streamID)
	bytesOut := buf.Len()

	if _, err := h.conn.Write(buf.Bytes()); err != nil {
		return "", "", err
	}

	// Request is marked as send and forget
	if !waitRsp {
		return "", "", nil
	}

	// Read response
	_, resp, status, bytesIn, err := h.readResponse(streamID)
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

// sendBatchH2 - sends multiple requests in single TCP write (H2 multiplexing)
// Returns streamIDs for response correlation
func (h *H2Conn) sendBatchH2(reqs []*ParsedReq) (streamIDs []uint32, bytesOut int, err error) {
	if err := h.handshake(); err != nil {
		return nil, 0, err
	}

	streamIDs = make([]uint32, len(reqs))
	var buf bytes.Buffer

	for i, req := range reqs {
		streamID := h.streamID
		h.streamID += 2
		streamIDs[i] = streamID
		h.encodeReqFrames(&buf, req, streamID)
	}

	if _, err := h.conn.Write(buf.Bytes()); err != nil {
		return nil, 0, err
	}

	return streamIDs, buf.Len(), nil
}

// readResponse - reads frames until END_STREAM for given streamID
// If streamID=0, accepts any stream (first HEADERS/DATA defines it)
// Returns: respStreamID, resp body, status code, bytes read, error
func (h *H2Conn) readResponse(streamID uint32) (respStreamID uint32, resp, status string, bytesIn int, err error) {
	var headers [][2]string
	var body bytes.Buffer
	var encoding string

	respStreamID = streamID // if 0, will be set by first frame

	// Reuse header buffer across frame reads
	hdr := make([]byte, 9)

	for {
		n, err := io.ReadFull(h.conn, hdr)
		bytesIn += n
		if err != nil {
			return 0, "", "", bytesIn, err
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
				return 0, "", "", bytesIn, err
			}
		}

		// NOTE: Server push (PUSH_PROMISE) ignored - frames discarded
		// Most APIs don't use push; browsers are primary consumers

		switch ftype {
		case frameHeaders, frameContinuation:
			// If streamID=0, accept any stream (first defines respStreamID)
			if respStreamID == 0 {
				respStreamID = fStreamID
			}
			if fStreamID != respStreamID {
				continue // different stream, skip
			}

			// Decode HPACK
			hdrs, err := h.dec.DecodeFull(payload)
			if err != nil {
				return 0, "", "", bytesIn, fmt.Errorf("hpack decode: %v", err)
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
			// If streamID=0, accept any stream (first defines respStreamID)
			if respStreamID == 0 {
				respStreamID = fStreamID
			}
			if fStreamID != respStreamID {
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
			return 0, "", "", bytesIn, fmt.Errorf("GOAWAY received")

		case frameRstStream:
			if fStreamID == respStreamID {
				// 4 bytes into correct endian
				errCode := uint32(payload[0])<<24 | uint32(payload[1])<<16 | uint32(payload[2])<<8 | uint32(payload[3])
				return 0, "", "", bytesIn, fmt.Errorf("RST_STREAM: %d", errCode)
			}

		case framePushPromise:
			// Server push ignored - just consume and discard
			continue
		}

		// Check END_STREAM on this stream
		if fStreamID == respStreamID && (flags&flagEndStream) != 0 {
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
	decodedBytes, err := decodeBody(&body, encoding)
	if err != nil {
		return 0, "", "", bytesIn, err
	}
	sb.Write(decodedBytes)

	return respStreamID, sb.String(), status, bytesIn, nil
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

// sendOnConnH2 - sends on existing H2 connection
func sendOnConnH2(raw []byte, h2 *H2Conn, threadID int) (resp, status string, err error) {
	req := parseRawReq2H2(string(raw))
	if req == nil {
		return "", "", fmt.Errorf("failed to parse request")
	}
	return h2.sendReqH2(req, threadID, true)
}
