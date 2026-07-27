package omp

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

const maxReassembledFrameBytes = 64 << 20

type frameDecoder struct {
	scanner *bufio.Scanner
	chunk   *chunkSequence
}

type chunkSequence struct {
	id         string
	count      int
	byteLength int
	nextIndex  int
	data       []byte
}

type chunkFrame struct {
	Type       string `json:"type"`
	ChunkID    string `json:"chunkId"`
	Index      int    `json:"index"`
	Count      int    `json:"count"`
	ByteLength int    `json:"byteLength"`
	Data       string `json:"data"`
}

func newFrameDecoder(reader io.Reader) *frameDecoder {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), maxFrameBytes)
	return &frameDecoder{scanner: scanner}
}

func (d *frameDecoder) Next() (map[string]any, error) {
	for d.scanner.Scan() {
		physical := append([]byte(nil), d.scanner.Bytes()...)
		var header struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(physical, &header); err != nil {
			return nil, fmt.Errorf("decode RPC frame header: %w", err)
		}
		if header.Type != "rpc_chunk" {
			if d.chunk != nil {
				return nil, errors.New("RPC chunk sequence was interrupted")
			}
			return decodeLogicalFrame(physical)
		}

		logical, err := d.acceptChunk(physical)
		if err != nil {
			return nil, err
		}
		if logical != nil {
			return decodeLogicalFrame(logical)
		}
	}
	if err := d.scanner.Err(); err != nil {
		return nil, fmt.Errorf("read RPC frame: %w", err)
	}
	if d.chunk != nil {
		return nil, errors.New("RPC chunk sequence ended before completion")
	}
	return nil, io.EOF
}

func (d *frameDecoder) acceptChunk(physical []byte) ([]byte, error) {
	var frame chunkFrame
	if err := json.Unmarshal(physical, &frame); err != nil {
		return nil, fmt.Errorf("decode RPC chunk: %w", err)
	}
	if frame.ChunkID == "" || frame.Count <= 0 || frame.Index < 0 || frame.Index >= frame.Count {
		return nil, errors.New("invalid RPC chunk identity or bounds")
	}
	if frame.ByteLength < 0 || frame.ByteLength > maxReassembledFrameBytes {
		return nil, fmt.Errorf("RPC chunk byteLength %d exceeds limit", frame.ByteLength)
	}
	if d.chunk == nil {
		if frame.Index != 0 {
			return nil, fmt.Errorf("RPC chunk sequence starts at index %d", frame.Index)
		}
		d.chunk = &chunkSequence{
			id:         frame.ChunkID,
			count:      frame.Count,
			byteLength: frame.ByteLength,
			data:       make([]byte, 0, frame.ByteLength),
		}
	}
	sequence := d.chunk
	if frame.ChunkID != sequence.id || frame.Count != sequence.count || frame.ByteLength != sequence.byteLength {
		return nil, errors.New("RPC chunk metadata changed within a sequence")
	}
	if frame.Index != sequence.nextIndex {
		return nil, fmt.Errorf("RPC chunk index %d received, want %d", frame.Index, sequence.nextIndex)
	}
	decoded, err := base64.StdEncoding.DecodeString(frame.Data)
	if err != nil {
		return nil, fmt.Errorf("decode RPC chunk data: %w", err)
	}
	if len(sequence.data)+len(decoded) > sequence.byteLength || len(sequence.data)+len(decoded) > maxReassembledFrameBytes {
		return nil, errors.New("RPC chunk data exceeds declared byteLength")
	}
	sequence.data = append(sequence.data, decoded...)
	sequence.nextIndex++
	if sequence.nextIndex != sequence.count {
		return nil, nil
	}
	if len(sequence.data) != sequence.byteLength {
		return nil, fmt.Errorf("reassembled RPC frame is %d bytes, want %d", len(sequence.data), sequence.byteLength)
	}
	if !utf8.Valid(sequence.data) {
		return nil, errors.New("reassembled RPC frame is not valid UTF-8")
	}
	logical := sequence.data
	d.chunk = nil
	return logical, nil
}

func decodeLogicalFrame(encoded []byte) (map[string]any, error) {
	var frame map[string]any
	if err := json.Unmarshal(encoded, &frame); err != nil {
		return nil, fmt.Errorf("decode RPC frame: %w", err)
	}
	return frame, nil
}
