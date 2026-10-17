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

// Pre-built SETTINGS frame
// Settings: MAX_CONCURRENT_STREAMS=100, INITIAL_WINDOW_SIZE=10485760, ENABLE_PUSH=0
var h2SettingsFrame = []byte{
	0, 0, 18, // length: 18 bytes (3 settings * 6 bytes)
	frameSettings, 0, // type=SETTINGS, flags=0
	0, 0, 0, 0, // stream ID = 0
	// SETTINGS_MAX_CONCURRENT_STREAMS = 100
	0, settingsMaxConcurrentStreams, 0, 0, 0, 100,
	// SETTINGS_INITIAL_WINDOW_SIZE = 10485760 (10MB)
	0, settingsInitialWindowSize, 0, 0xa0, 0, 0,
	// SETTINGS_ENABLE_PUSH = 0
	0, settingsEnablePush, 0, 0, 0, 0,
}

// Pre-built WINDOW_UPDATE
// Increment: 1048510465 (~1GB) Yep :). Protocol allows, but TREM don't need to comply \o/
var h2WindowUpdate = []byte{
	0, 0, 4, // length: 4 bytes
	frameWindowUpdate, 0, // type=WINDOW_UPDATE, flags=0
	0, 0, 0, 0, // stream ID = 0 (connection-level)
	0x3e, 0x7f, 0x00, 0x01, // increment = 1048510465
}

// h2HandshakeData - pre-built handshake buffer (preface + settings + window update)
// Initialized once at startup to avoid repeated allocations and copies
var h2HandshakeData []byte

// init - default single allocated copy-on-write on h2HandshakeData for H2 handshake
func init() {
	// Pre-allocate and copy all handshake data into single buffer
	h2HandshakeData = make([]byte, len(h2Preface)+len(h2SettingsFrame)+len(h2WindowUpdate))
	n := copy(h2HandshakeData, h2Preface)
	n += copy(h2HandshakeData[n:], h2SettingsFrame)
	copy(h2HandshakeData[n:], h2WindowUpdate)
}

// h2Resp - keeps one stream's response until it is delivered
type h2Resp struct {
	headers  [][2]string
	body     bytes.Buffer
	status   string
	encoding string
	bytesIn  int
	done     bool
	err      error
}

// H2Conn - wraps net.Conn with HTTP/2 state
type H2Conn struct {
	conn      net.Conn
	enc       *hpack.Encoder
	dec       *hpack.Decoder
	encBuf    bytes.Buffer
	streamID  uint32
	handshook bool
	streams   map[uint32]*h2Resp
	ready     []uint32
	ctrlIn    int
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

// nextStream - registers an outgoing stream before its frames are written
func (h *H2Conn) nextStream() uint32 {
	id := h.streamID
	h.streamID += 2 // client streams are odd
	if h.streams == nil {
		h.streams = make(map[uint32]*h2Resp)
	}
	h.streams[id] = nil
	return id
}

// close - releases pending responses with their connection
func (h *H2Conn) close() {
	h.streams = nil
	h.ready = nil
	h.ctrlIn = 0
	h.conn.Close()
}

// handshake - sends connection preface and settings
// Uses pre-built h2HandshakeData buffer to avoid allocations
func (h *H2Conn) handshake() error {
	if h.handshook {
		return nil
	}

	// Single write with pre-built buffer (preface + settings + window update)
	if _, err := h.conn.Write(h2HandshakeData); err != nil {
		return fmt.Errorf("H2 handshake write: %w", err)
	}

	h.handshook = true
	return nil
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
	if !hasBody && len(req.Trailers) == 0 {
		flags |= flagEndStream
	}

	writeFrame(dst, frameHeaders, flags, streamID, headerBlock)

	if hasBody {
		flags := byte(0)
		if len(req.Trailers) == 0 {
			flags = flagEndStream
		}
		writeFrame(dst, frameData, flags, streamID, req.Body)
	}
	if len(req.Trailers) > 0 {
		h.encBuf.Reset()
		for _, hdr := range req.Trailers {
			h.enc.WriteField(hpack.HeaderField{Name: strings.ToLower(hdr[0]), Value: hdr[1]})
		}
		writeFrame(dst, frameHeaders, flagEndHeaders|flagEndStream, streamID, h.encBuf.Bytes())
	}
}

func (h *H2Conn) sendReqH2(req *ParsedReq, threadID int, waitRsp bool) (resp, status string, err error) {
	var startTime time.Time
	if verbose {
		startTime = time.Now()
	}

	if err := h.handshake(); err != nil {
		return "", "", err
	}

	streamID := h.nextStream()

	// Encode and send frames
	var buf bytes.Buffer
	h.encodeReqFrames(&buf, req, streamID)
	bytesOut, err := h.conn.Write(buf.Bytes())
	stats.ReportIO(threadID, 0, uint32(bytesOut))
	if err != nil {
		return "", "", err
	}

	// Request is marked as send and forget
	if !waitRsp {
		return "", "", nil
	}

	// Read response
	_, resp, status, bytesIn, err := h.readResponse(streamID)
	stats.ReportIO(threadID, uint32(bytesIn), 0)
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
	stats.ReportRequest(threadID, latencyUs, 0, 0)
	return resp, status, nil
}

// sendBatchH2 - sends multiple requests in single TCP write (H2 multiplexing)
// Returns streamIDs for response correlation
// bytesOut includes partial writes on error!!
func (h *H2Conn) sendBatchH2(reqs []*ParsedReq) (streamIDs []uint32, bytesOut int, err error) {
	if err := h.handshake(); err != nil {
		return nil, 0, err
	}

	streamIDs = make([]uint32, len(reqs))
	var buf bytes.Buffer

	for i, req := range reqs {
		streamID := h.nextStream()
		streamIDs[i] = streamID
		h.encodeReqFrames(&buf, req, streamID)
	}

	n, err := h.conn.Write(buf.Bytes())
	if err != nil {
		return nil, n, err
	}
	return streamIDs, n, nil
}

// framePayload - removes padding and optional HEADERS priority fields 
func framePayload(payload []byte, flags byte, priority bool) ([]byte, error) {
	if flags&flagPadded != 0 {
		if len(payload) == 0 || int(payload[0]) > len(payload)-1 {
			return nil, fmt.Errorf("invalid H2 padding")
		}
		payload = payload[1 : len(payload)-int(payload[0])]
	}
	if priority && flags&flagPriority != 0 {
		if len(payload) < 5 {
			return nil, fmt.Errorf("invalid H2 priority")
		}
		payload = payload[5:]
	}
	return payload, nil
}

// response - delivers and releases one completed stream, counting control bytes once.
func (h *H2Conn) response(id uint32) (uint32, string, string, int, error) {
	r := h.streams[id]
	delete(h.streams, id)
	if len(h.streams) == 0 {
		h.streams = nil
		h.ready = nil
	}
	n := r.bytesIn + h.ctrlIn
	h.ctrlIn = 0
	if r.err != nil {
		return id, "", "", n, r.err
	}

	// Build response string (headers + body)
	var sb strings.Builder
	for _, hdr := range r.headers {
		// Normalize H2 header names for HTTP/1.1 regex compatibility
		sb.WriteString(toTitleCase(hdr[0]))
		sb.WriteString(": ")
		sb.WriteString(hdr[1])
		sb.WriteString("\r\n")
	}
	sb.WriteString("\r\n")

	// Decode body if compressed
	decodedBytes, err := decodeBody(&r.body, r.encoding)
	if err != nil {
		return id, "", "", n, err
	}
	sb.Write(decodedBytes)
	return id, sb.String(), r.status, n, nil
}

// readResponse - single reader; streamID=0 returns the next completed response 
func (h *H2Conn) readResponse(streamID uint32) (respStreamID uint32, resp, status string, bytesIn int, err error) {
	defer func() {
		if err != nil {
			h.close()
		}
	}()
	if streamID != 0 {
		r, ok := h.streams[streamID]
		if !ok {
			return 0, "", "", 0, fmt.Errorf("unknown H2 stream %d", streamID) 
		} 
		if r != nil && r.done {
			return h.response(streamID)
		}
	} else { 
		for len(h.ready) > 0 {
			id := h.ready[0]
			h.ready = h.ready[1:]
			if r := h.streams[id]; r != nil && r.done {
				return h.response(id)
			}
		}
	}

	firstRead := true
	var hdr [9]byte
	var hdrBuf bytes.Buffer
	var hdrID uint32
	var hdrEnd bool
	for {
		n, err := io.ReadFull(h.conn, hdr[:])
		bytesIn += n
		if err != nil {
			return 0, "", "", bytesIn, err
		}

		// First frame validation: detect HTTP/1.1 fallback or invalid H2
		if firstRead {
			if bytes.HasPrefix(hdr[:], []byte("HTTP/")) {
				return 0, "", "", bytesIn, fmt.Errorf("%w: server responded HTTP/1.1", errH2NotSupported)
			}
			if err := validateH2FrameHeader(hdr[:]); err != nil {
				return 0, "", "", bytesIn, err
			}
			firstRead = false
		}

		// H2 uses size in BIGEndian, lets bitwise it :)
		length := int(hdr[0])<<16 | int(hdr[1])<<8 | int(hdr[2])
		ftype, flags := hdr[3], hdr[4]
		fStreamID := binary.BigEndian.Uint32(hdr[5:]) & 0x7fffffff
		if hdrID != 0 && (ftype != frameContinuation || fStreamID != hdrID) {
			return 0, "", "", bytesIn, fmt.Errorf("invalid H2 continuation")
		}
		var payload []byte
		if length > 0 {
			payload = make([]byte, length)
			n, err = io.ReadFull(h.conn, payload)
			bytesIn += n
			if err != nil {
				return 0, "", "", bytesIn, err
			}
		}

		// NOTE: Server push (PUSH_PROMISE) ignored - frames discarded
		// Most APIs don't use push; browsers are primary consumers
		var r *h2Resp
		switch ftype {
		case frameHeaders, frameContinuation, frameData, frameRstStream:
			var ok bool
			r, ok = h.streams[fStreamID]
			// A completed stream may be followed by a NO_ERROR reset 
			if ftype == frameRstStream && length == 4 && binary.BigEndian.Uint32(payload) == 0 && fStreamID != 0 && fStreamID&1 != 0 && fStreamID < h.streamID && (!ok || r != nil && r.done) {
				h.ctrlIn += 9 + length
				continue
			}
			if !ok {
				return 0, "", "", bytesIn, fmt.Errorf("unknown H2 stream %d", fStreamID)
			}
			if r == nil {
				r = &h2Resp{}
				h.streams[fStreamID] = r
			}
			if r.done {
				return 0, "", "", bytesIn, fmt.Errorf("closed H2 stream %d", fStreamID)
			}
			r.bytesIn += 9 + length
		default:
			h.ctrlIn += 9 + length
		}

		switch ftype {
		case frameHeaders, frameContinuation:
			if ftype == frameHeaders {
				payload, err = framePayload(payload, flags, true)
				if err != nil {
					return 0, "", "", bytesIn, err
				}
				hdrEnd = flags&flagEndStream != 0
				if flags&flagEndHeaders == 0 {
					hdrID = fStreamID
					hdrBuf.Write(payload)
					continue
				}
			} else {
				if hdrID == 0 {
					return 0, "", "", bytesIn, fmt.Errorf("unexpected H2 continuation")
				}
				hdrBuf.Write(payload)
				if flags&flagEndHeaders == 0 {
					continue
				}
				payload = hdrBuf.Bytes()
				hdrID = 0
			}

			// Decode complete header blocks in connection order to preserve HPACK state
			hdrs, err := h.dec.DecodeFull(payload)
			if err != nil {
				return 0, "", "", bytesIn, fmt.Errorf("hpack decode: %v", err)
			}
			for _, hf := range hdrs {
				if hf.Name == ":status" {
					r.status = hf.Value
				} else {
					r.headers = append(r.headers, [2]string{hf.Name, hf.Value})
					if hf.Name == "content-encoding" {
						r.encoding = hf.Value
					}
				}
			}
			hdrBuf.Reset()
			r.done = hdrEnd

		case frameData:
			payload, err = framePayload(payload, flags, false)
			if err != nil {
				return 0, "", "", bytesIn, err
			}
			r.body.Write(payload)
			r.done = flags&flagEndStream != 0

		case frameSettings:
			// Server SETTINGS - ACK if not already ACK
			if flags&0x1 == 0 {
				_, err = h.conn.Write(buildFrame(frameSettings, 0x1, 0, nil))
				if err != nil {
					return 0, "", "", bytesIn, err
				}
			}
			continue

		case frameWindowUpdate:
			// Ignored - we don't track send window
			continue

		case framePing:
			// Respond to PING
			if flags&0x1 == 0 {
				_, err = h.conn.Write(buildFrame(framePing, 0x1, 0, payload))
				if err != nil {
					return 0, "", "", bytesIn, err
				}
			}
			continue

		case frameGoAway:
			errCode := uint32(0)
			if len(payload) >= 8 {
				errCode = binary.BigEndian.Uint32(payload[4:])
			}
			return 0, "", "", bytesIn, fmt.Errorf("GOAWAY received, error code: %d", errCode)

		case frameRstStream:
			if len(payload) != 4 {
				return 0, "", "", bytesIn, fmt.Errorf("invalid H2 reset")
			}
			r.err = fmt.Errorf("RST_STREAM: %d", binary.BigEndian.Uint32(payload))
			r.done = true

		case framePushPromise:
			// Push is disabled in the client SETTINGS.
			return 0, "", "", bytesIn, fmt.Errorf("unexpected H2 push")

		default:
			continue
		}

		if r.done {
			if streamID == 0 || fStreamID == streamID {
				return h.response(fStreamID)
			}
			h.ready = append(h.ready, fStreamID)
		}
	}
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

// writeFrame - appends a frame directly to the batch buffer without a temporary payload copy.
func writeFrame(dst *bytes.Buffer, ftype, flags byte, streamID uint32, payload []byte) {
	length := len(payload)
	hdr := [9]byte{byte(length >> 16), byte(length >> 8), byte(length), ftype, flags}
	binary.BigEndian.PutUint32(hdr[5:], streamID)
	dst.Grow(len(hdr) + length)
	dst.Write(hdr[:])
	dst.Write(payload)
}

// sendOnConnH2 - parses raw text and sends on an existing H2 connection.
func sendOnConnH2(raw string, h2 *H2Conn, threadID int) (resp, status string, err error) {
	req, err := parseRawReq2H2(raw)
	if err != nil {
		return "", "", err
	}
	return h2.sendReqH2(req, threadID, true)
}
