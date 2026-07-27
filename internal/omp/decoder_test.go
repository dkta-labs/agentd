package omp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestFrameDecoderReassemblesProtocolV2Chunks(t *testing.T) {
	logical := []byte(`{"type":"message_update","payload":"` + strings.Repeat("x", 200) + `"}`)
	first := logical[:90]
	second := logical[90:]
	input := bytes.NewBuffer(nil)
	for index, part := range [][]byte{first, second} {
		frame := chunkFrame{
			Type:       "rpc_chunk",
			ChunkID:    "chunk-1",
			Index:      index,
			Count:      2,
			ByteLength: len(logical),
			Data:       base64.StdEncoding.EncodeToString(part),
		}
		if err := json.NewEncoder(input).Encode(frame); err != nil {
			t.Fatal(err)
		}
	}

	decoded, err := newFrameDecoder(input).Next()
	if err != nil {
		t.Fatal(err)
	}
	if decoded["type"] != "message_update" || decoded["payload"] != strings.Repeat("x", 200) {
		t.Fatalf("unexpected decoded frame: %#v", decoded)
	}
}

func TestFrameDecoderRejectsInterruptedChunkSequence(t *testing.T) {
	input := bytes.NewBuffer(nil)
	if err := json.NewEncoder(input).Encode(chunkFrame{
		Type:       "rpc_chunk",
		ChunkID:    "chunk-1",
		Index:      0,
		Count:      2,
		ByteLength: 10,
		Data:       base64.StdEncoding.EncodeToString([]byte("half")),
	}); err != nil {
		t.Fatal(err)
	}
	input.WriteString("{\"type\":\"notice\"}\n")

	if _, err := newFrameDecoder(input).Next(); err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("error = %v, want interrupted sequence", err)
	}
}
