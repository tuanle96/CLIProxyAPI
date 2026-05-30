package executor

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
)

// buildEventStreamFrame encodes a single AWS Event Stream frame with a
// :event-type string header and a JSON payload, matching the layout
// readEventStreamMessage expects: prelude(12) + headers + payload + crc(4).
func buildEventStreamFrame(eventType string, payload []byte) []byte {
	// Header: [nameLen:1][name][valueType:1=7][valueLen:2][value]
	name := ":event-type"
	var hdr bytes.Buffer
	hdr.WriteByte(byte(len(name)))
	hdr.WriteString(name)
	hdr.WriteByte(7) // string type
	var vl [2]byte
	binary.BigEndian.PutUint16(vl[:], uint16(len(eventType)))
	hdr.Write(vl[:])
	hdr.WriteString(eventType)

	headers := hdr.Bytes()
	total := 12 + len(headers) + len(payload) + 4

	frame := make([]byte, 0, total)
	var prelude [12]byte
	binary.BigEndian.PutUint32(prelude[0:4], uint32(total))
	binary.BigEndian.PutUint32(prelude[4:8], uint32(len(headers)))
	// prelude[8:12] CRC unchecked
	frame = append(frame, prelude[:]...)
	frame = append(frame, headers...)
	frame = append(frame, payload...)
	frame = append(frame, 0, 0, 0, 0) // message CRC (unchecked)
	return frame
}

// stallReader yields the configured bytes once, then returns a non-EOF error to
// simulate the idle watchdog closing a stalled upstream body mid-stream.
type stallReader struct {
	data []byte
	done bool
}

func (s *stallReader) Read(p []byte) (int, error) {
	if !s.done {
		n := copy(p, s.data)
		s.data = s.data[n:]
		if len(s.data) == 0 {
			s.done = true
		}
		return n, nil
	}
	return 0, errors.New("read tcp: use of closed network connection")
}

// TestStreamToChannelFinalizesOnMidStreamError verifies that when the upstream
// stalls/errors after streaming has started, streamToChannel emits a terminal
// message_stop instead of only an error, so the Responses chain can complete.
func TestStreamToChannelFinalizesOnMidStreamError(t *testing.T) {
	frame := buildEventStreamFrame("assistantResponseEvent", []byte(`{"assistantResponseEvent":{"content":"Hello"}}`))
	reader := &stallReader{data: frame}

	e := NewKiroExecutor(&config.Config{})
	out := make(chan cliproxyexecutor.StreamChunk, 64)
	reporter := helps.NewUsageReporter(context.Background(), "kiro", "claude-sonnet-4-5", nil)

	go func() {
		defer close(out)
		e.streamToChannel(
			context.Background(),
			reader,
			out,
			sdktranslator.FromString("claude"),
			"claude-sonnet-4-5",
			[]byte(`{"model":"claude-sonnet-4-5","messages":[]}`),
			[]byte(`{"model":"claude-sonnet-4-5","messages":[]}`),
			reporter,
			true,
		)
	}()

	var got strings.Builder
	var sawErr bool
	timeout := time.After(10 * time.Second)
	for {
		select {
		case chunk, ok := <-out:
			if !ok {
				goto done
			}
			if chunk.Err != nil {
				sawErr = true
			}
			got.Write(chunk.Payload)
		case <-timeout:
			t.Fatal("streamToChannel did not finalize within 10s (hang)")
		}
	}
done:
	body := got.String()
	if !strings.Contains(body, "message_stop") {
		t.Errorf("expected a terminal message_stop after mid-stream error, got:\n%s", body)
	}
	if sawErr {
		t.Errorf("expected graceful finalization (no raw error chunk) after content was streamed, but an error chunk was emitted:\n%s", body)
	}
	if !strings.Contains(body, "Hello") {
		t.Errorf("expected the partial content to be delivered, got:\n%s", body)
	}
}
