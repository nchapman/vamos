package agentproto

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Typed stream frame kinds used after a successful session response. Control
// information (exit status) is a distinct frame kind from workload data, so
// container output can never forge a terminal status.
const (
	StreamStdout  byte = 0x01
	StreamControl byte = 0x02
	StreamEOF     byte = 0x03
	StreamStderr  byte = 0x04
	StreamInput   byte = 0x05
	StreamResize  byte = 0x06
)

const (
	MaxStreamChunk   = 32 * 1024
	maxDecodeChunk   = 1 << 20
	streamHeaderSize = 5
)

var ErrStreamClosed = errors.New("agent session stream closed")

// EncodeStreamChunk writes one typed length-prefixed frame. Data frames carry
// workload bytes; the control kind carries strict JSON.
func EncodeStreamChunk(w io.Writer, kind byte, data []byte) error {
	if kind == StreamEOF || kind == StreamControl || kind == StreamResize {
		if len(data) > MaxEnvelopeBytes {
			return fmt.Errorf("agent stream control frame exceeds %d bytes", MaxEnvelopeBytes)
		}
	} else if len(data) > MaxStreamChunk {
		return fmt.Errorf("agent stream chunk exceeds %d bytes", MaxStreamChunk)
	}
	header := [streamHeaderSize]byte{kind, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(header[1:], uint32(len(data)))
	if _, err := w.Write(header[:]); err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	_, err := w.Write(data)
	return err
}

// DecodeStreamChunk reads one typed frame, enforcing a bounded chunk size so a
// hostile peer cannot force unbounded allocations.
func DecodeStreamChunk(r io.Reader) (kind byte, data []byte, err error) {
	header := [streamHeaderSize]byte{}
	if _, err := io.ReadFull(r, header[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return 0, nil, ErrStreamClosed
		}
		return 0, nil, err
	}
	kind = header[0]
	size := binary.BigEndian.Uint32(header[1:])
	if kind != StreamStdout && kind != StreamControl && kind != StreamEOF && kind != StreamStderr && kind != StreamInput && kind != StreamResize {
		return 0, nil, errors.New("invalid agent stream frame kind")
	}
	if size > maxDecodeChunk {
		return 0, nil, errors.New("agent stream frame exceeds decode limit")
	}
	if kind == StreamEOF && size != 0 {
		return 0, nil, errors.New("invalid agent stream EOF frame")
	}
	if (kind == StreamInput || kind == StreamResize) && size == 0 {
		return 0, nil, errors.New("invalid agent stream input frame")
	}
	data = make([]byte, size)
	if _, err := io.ReadFull(r, data); err != nil {
		return 0, nil, err
	}
	return kind, data, nil
}

func EncodeStreamResize(w io.Writer, size WindowSize) error {
	data, err := json.Marshal(size)
	if err != nil {
		return err
	}
	return EncodeStreamChunk(w, StreamResize, data)
}

func DecodeStreamResize(payload []byte) (WindowSize, error) {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	var size WindowSize
	if err := dec.Decode(&size); err != nil {
		return WindowSize{}, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return WindowSize{}, errors.New("trailing session resize data")
	}
	return size, nil
}

func DecodeSessionExit(payload []byte) (SessionExit, error) {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	var exit SessionExit
	if err := dec.Decode(&exit); err != nil {
		return SessionExit{}, err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return SessionExit{}, errors.New("trailing session control data")
	}
	return exit, nil
}
