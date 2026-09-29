package ws

import (
	"bytes"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	lengths := []int{0, 125, 126, 65535, 65536}
	for _, masked := range []bool{false, true} {
		for _, length := range lengths {
			t.Run(maskLabel(masked)+"/"+strconv.Itoa(length), func(t *testing.T) {
				payload := make([]byte, length)
				for i := range payload {
					payload[i] = byte(i % 251)
				}
				in := Frame{FIN: true, Opcode: OpcodeBinary, Payload: payload}

				var buf bytes.Buffer
				if err := writeFrame(&buf, in, masked); err != nil {
					t.Fatalf("writeFrame error = %v, want nil", err)
				}
				got, err := readFrame(&buf, int64(length)+1)
				if err != nil {
					t.Fatalf("readFrame error = %v, want nil", err)
				}
				if got.FIN != in.FIN {
					t.Errorf("FIN = %v, want %v", got.FIN, in.FIN)
				}
				if got.Opcode != in.Opcode {
					t.Errorf("Opcode = 0x%x, want 0x%x", got.Opcode, in.Opcode)
				}
				if !bytes.Equal(got.Payload, payload) {
					t.Errorf("Payload differs (got %d bytes, want %d)", len(got.Payload), len(payload))
				}
			})
		}
	}
}

func maskLabel(masked bool) string {
	if masked {
		return "masked"
	}
	return "unmasked"
}

// TestLengthForms pins the header wire shape for each of the three
// payload-length forms: 7-bit base up to 125, 16-bit from 126, and 64-bit
// above 65535.
func TestLengthForms(t *testing.T) {
	tests := []struct {
		length int
		want   []byte // header bytes before the mask key and payload
	}{
		{length: 0, want: []byte{0x81, 0x00}},
		{length: 125, want: []byte{0x81, 0x7d}},
		{length: 126, want: []byte{0x81, 126, 0x00, 0x7e}},
		{length: 127, want: []byte{0x81, 126, 0x00, 0x7f}},
		{length: 128, want: []byte{0x81, 126, 0x00, 0x80}},
		{length: 65535, want: []byte{0x81, 126, 0xff, 0xff}},
		{length: 65536, want: []byte{0x81, 127, 0x00, 0x00, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00}},
	}
	for _, tt := range tests {
		var buf bytes.Buffer
		payload := make([]byte, tt.length)
		if err := writeFrame(&buf, Frame{FIN: true, Opcode: OpcodeText, Payload: payload}, false); err != nil {
			t.Fatalf("writeFrame(%d) error = %v, want nil", tt.length, err)
		}
		encoded := buf.Bytes()
		if !bytes.Equal(encoded[:len(tt.want)], tt.want) {
			t.Errorf("length %d: header = %x, want %x", tt.length, encoded[:len(tt.want)], tt.want)
		}
	}
}

func TestMasking(t *testing.T) {
	payload := []byte("payload bytes here")

	var buf bytes.Buffer
	if err := writeFrame(&buf, Frame{FIN: true, Opcode: OpcodeText, Payload: payload}, true); err != nil {
		t.Fatalf("writeFrame error = %v, want nil", err)
	}

	raw := buf.Bytes()
	// After the 2-byte header and 4-byte key, the masked payload on the
	// wire differs from the original.
	maskedWire := raw[6:]
	if bytes.Equal(maskedWire, payload) {
		t.Error("wire bytes match the plain payload; masking did nothing")
	}

	got, err := readFrame(bytes.NewReader(raw), int64(len(payload)))
	if err != nil {
		t.Fatalf("readFrame error = %v, want nil", err)
	}
	if !bytes.Equal(got.Payload, payload) {
		t.Errorf("decoded payload = %q, want %q", got.Payload, payload)
	}
}

func TestMaskKeysDiffer(t *testing.T) {
	var firstKey [4]byte
	for i := 0; i < 2; i++ {
		var buf bytes.Buffer
		if err := writeFrame(&buf, Frame{FIN: true, Opcode: OpcodeText, Payload: []byte("x")}, true); err != nil {
			t.Fatalf("writeFrame error = %v, want nil", err)
		}
		var key [4]byte
		copy(key[:], buf.Bytes()[2:6])
		if i == 0 {
			firstKey = key
			continue
		}
		if key == firstKey {
			t.Errorf("two frames written with the same mask key %v; randomness is broken", key)
		}
	}
}

// TestServerFrameDecodes decodes a frame a server sent: servers never
// mask.
func TestServerFrameDecodes(t *testing.T) {
	raw := []byte{0x81, 0x03, 'a', 'b', 'c'}
	got, err := readFrame(bytes.NewReader(raw), 1024)
	if err != nil {
		t.Fatalf("readFrame error = %v, want nil", err)
	}
	if got.Opcode != OpcodeText || string(got.Payload) != "abc" || !got.FIN {
		t.Errorf("readFrame = %+v, want FIN text \"abc\"", got)
	}
}

func TestInvalidFrames(t *testing.T) {
	tests := []struct {
		name string
		raw  []byte
		max  int64
		want string
	}{
		{
			name: "nonzero rsv bit",
			raw:  []byte{0xC1, 0x01, 'x'},
			max:  1024,
			want: "RSV",
		},
		{
			name: "fragmented close",
			raw:  []byte{0x08, 0x00},
			max:  1024,
			want: "must not be fragmented",
		},
		{
			name: "control frame over 125 bytes",
			raw:  []byte{0x89, 126, 0x00, 0x7e, 'x'},
			max:  1024,
			want: "exceeds 125",
		},
		{
			name: "payload over maxLen",
			raw:  []byte{0x81, 0x7e, 0x01, 0x00, 'x'},
			max:  4,
			want: "exceeds read limit",
		},
		{
			name: "truncated header",
			raw:  []byte{0x81},
			max:  1024,
			want: "read frame header",
		},
		{
			name: "truncated payload",
			raw:  []byte{0x81, 0x05, 'a'},
			max:  1024,
			want: "read 5-byte frame payload",
		},
		{
			name: "length announced but payload missing",
			raw:  []byte{0x81, 0x7e, 0x01, 0x00},
			max:  1024,
			want: "read 256-byte frame payload",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := readFrame(bytes.NewReader(tt.raw), tt.max)
			if err == nil {
				t.Fatalf("readFrame error = nil, want it to contain %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("readFrame error = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestRSVErrorIsSentinel(t *testing.T) {
	_, err := readFrame(bytes.NewReader([]byte{0xA1, 0x00}), 1024)
	if !errors.Is(err, ErrRSVBit) {
		t.Errorf("error = %v, want ErrRSVBit", err)
	}
}

// oneByteReader returns every byte one at a time, exercising the
// io.Reader loop over short reads.
type oneByteReader struct {
	r io.Reader
}

func (o oneByteReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return o.r.Read(p[:1])
}

func TestShortReads(t *testing.T) {
	var buf bytes.Buffer
	if err := writeFrame(&buf, Frame{FIN: true, Opcode: OpcodeText, Payload: []byte("hello masked world")}, true); err != nil {
		t.Fatalf("writeFrame error = %v, want nil", err)
	}
	frame, err := readFrame(oneByteReader{&buf}, 1024)
	if err != nil {
		t.Fatalf("readFrame error = %v, want nil under one-byte short reads", err)
	}
	if string(frame.Payload) != "hello masked world" {
		t.Errorf("Payload = %q, want %q", frame.Payload, "hello masked world")
	}
}

func TestWriteControlValidation(t *testing.T) {
	var buf bytes.Buffer
	long := make([]byte, 126)
	if err := writeFrame(&buf, Frame{FIN: true, Opcode: OpcodePing, Payload: long}, false); err == nil {
		t.Error("writeFrame error = nil for an over-long ping, want error")
	}
	if buf.Len() != 0 {
		t.Errorf("%d bytes written for a rejected frame, want none", buf.Len())
	}

	var buf2 bytes.Buffer
	frag := Frame{FIN: false, Opcode: OpcodeClose}
	if err := writeFrame(&buf2, frag, false); err == nil {
		t.Error("writeFrame error = nil for a fragmented close, want error")
	}
	if buf2.Len() != 0 {
		t.Errorf("%d bytes written for a rejected frame, want none", buf2.Len())
	}
}
