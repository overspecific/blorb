// Package ws implements the small slice of RFC 6455 blorb needs: a frame
// codec plus a client-side dialer and connection, with no dependencies
// beyond the standard library.
package ws

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Opcode is a WebSocket frame operation code (RFC 6455 section 5.2).
type Opcode byte

const (
	// OpcodeContinuation is a continuation frame of a fragmented message.
	OpcodeContinuation Opcode = 0x0
	// OpcodeText is a UTF-8 text data frame.
	OpcodeText Opcode = 0x1
	// OpcodeBinary is a binary data frame.
	OpcodeBinary Opcode = 0x2
	// OpcodeClose is a connection close control frame.
	OpcodeClose Opcode = 0x8
	// OpcodePing is a ping control frame.
	OpcodePing Opcode = 0x9
	// OpcodePong is a pong control frame.
	OpcodePong Opcode = 0xA
)

// control reports whether the opcode is a control frame (close, ping,
// pong). Control frames are small, must not be fragmented, and may
// interleave with fragmented data messages.
func (o Opcode) control() bool {
	return o >= 0x8
}

// Frame is one WebSocket frame header plus its (unmasked) payload.
type Frame struct {
	// FIN reports whether this is the final fragment of a message.
	FIN bool
	// Opcode is the frame operation code.
	Opcode Opcode
	// Payload is the frame payload, unmasked.
	Payload []byte
}

// maxControlPayload is the largest control frame payload RFC 6455 allows
// (fits in the 7-bit base length form).
const maxControlPayload = 125

// ErrRSVBit is returned when a frame carries a nonzero RSV bit. blorb
// speaks plain RFC 6455 with no extensions, so no RSV bit is ever valid.
var ErrRSVBit = errors.New("websocket frame has nonzero RSV bits (no extensions negotiated)")

// readFrame reads exactly one frame from r and returns it, with any mask
// stripped. maxLen bounds the payload size: a larger frame errors before
// any payload bytes are read, protecting against a peer announcing a huge
// length.
func readFrame(r io.Reader, maxLen int64) (Frame, error) {
	var head [2]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return Frame{}, fmt.Errorf("read frame header: %w", err)
	}

	fin := head[0]&0x80 != 0
	if rsv := head[0] & 0x70; rsv != 0 {
		return Frame{}, fmt.Errorf("%w: byte 0x%02x", ErrRSVBit, rsv>>4)
	}
	op := Opcode(head[0] & 0x0f)

	masked := head[1]&0x80 != 0
	length := int64(head[1] & 0x7f)

	if op.control() {
		if !fin {
			return Frame{}, fmt.Errorf("control frame opcode 0x%x must not be fragmented", op)
		}
		if length > maxControlPayload {
			return Frame{}, fmt.Errorf("control frame opcode 0x%x payload %d exceeds %d bytes", op, length, maxControlPayload)
		}
	}

	if length == 126 {
		var ext [2]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return Frame{}, fmt.Errorf("read 16-bit frame length: %w", err)
		}
		length = int64(binary.BigEndian.Uint16(ext[:]))
	} else if length == 127 {
		var ext [8]byte
		if _, err := io.ReadFull(r, ext[:]); err != nil {
			return Frame{}, fmt.Errorf("read 64-bit frame length: %w", err)
		}
		length = int64(binary.BigEndian.Uint64(ext[:]))
		if length < 0 {
			return Frame{}, fmt.Errorf("frame length %d overflows int64", length)
		}
	}

	if length > maxLen {
		return Frame{}, fmt.Errorf("frame payload %d exceeds read limit %d", length, maxLen)
	}

	var key [4]byte
	if masked {
		if _, err := io.ReadFull(r, key[:]); err != nil {
			return Frame{}, fmt.Errorf("read mask key: %w", err)
		}
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return Frame{}, fmt.Errorf("read %d-byte frame payload: %w", length, err)
	}
	if masked {
		mask(key, payload)
	}

	return Frame{FIN: fin, Opcode: op, Payload: payload}, nil
}

// writeFrame writes one frame to w. When masked, a random key is
// generated and the payload is masked client-side per RFC 6455 section
// 5.3; client connections always mask, test servers do not.
func writeFrame(w io.Writer, f Frame, masked bool) error {
	if f.Opcode.control() {
		if !f.FIN {
			return fmt.Errorf("control frame opcode 0x%x must not be fragmented", f.Opcode)
		}
		if len(f.Payload) > maxControlPayload {
			return fmt.Errorf("control frame opcode 0x%x payload %d exceeds %d bytes", f.Opcode, len(f.Payload), maxControlPayload)
		}
	}

	var head [14]byte
	head[0] = byte(f.Opcode)
	if f.FIN {
		head[0] |= 0x80
	}

	length := int64(len(f.Payload))
	maskBit := byte(0)
	if masked {
		maskBit = 0x80
	}
	n := 2
	switch {
	case length <= 125:
		head[1] = maskBit | byte(length)
	case length <= 65535:
		head[1] = maskBit | 126
		binary.BigEndian.PutUint16(head[2:], uint16(length))
		n += 2
	default:
		head[1] = maskBit | 127
		binary.BigEndian.PutUint64(head[2:], uint64(length))
		n += 8
	}

	if masked {
		var key [4]byte
		if _, err := rand.Read(key[:]); err != nil {
			return fmt.Errorf("generate mask key: %w", err)
		}
		copy(head[n:], key[:])
		n += 4
		maskedPayload := append([]byte(nil), f.Payload...)
		mask(key, maskedPayload)
		if _, err := w.Write(head[:n]); err != nil {
			return err
		}
		_, err := w.Write(maskedPayload)
		return err
	}

	if _, err := w.Write(head[:n]); err != nil {
		return err
	}
	_, err := w.Write(f.Payload)
	return err
}

// mask XORs the payload with the 4-byte key in place, per RFC 6455
// section 5.3: payload[i] ^= key[i % 4].
func mask(key [4]byte, payload []byte) {
	for i := range payload {
		payload[i] ^= key[i%4]
	}
}
