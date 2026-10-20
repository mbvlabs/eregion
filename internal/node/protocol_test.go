package node

import (
	"bufio"
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/engine"
)

func TestFramesRoundTrip(t *testing.T) {
	var buffer bytes.Buffer
	if err := WriteFrame(&buffer, Frame{Type: FrameHello, Version: ProtocolVersion, Node: "app-1"}); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(&buffer, Frame{Type: FrameResponse, Response: &Response{Status: engine.StatusCompleted, Executed: true}}); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(&buffer)
	hello, err := ReadFrame(reader)
	if err != nil {
		t.Fatal(err)
	}
	if hello.Type != FrameHello || hello.Version != ProtocolVersion || hello.Node != "app-1" {
		t.Fatalf("hello frame: %+v", hello)
	}
	response, err := ReadFrame(reader)
	if err != nil {
		t.Fatal(err)
	}
	if response.Type != FrameResponse || response.Response == nil || response.Response.Status != engine.StatusCompleted {
		t.Fatalf("response frame: %+v", response)
	}
}

func TestReadFrameRejectsMalformedInput(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader("{\"type\":\"response\"\n"))
	if _, err := ReadFrame(reader); err == nil {
		t.Fatal("malformed frame was accepted")
	}
}

func TestReadFrameRejectsAnOversizedLine(t *testing.T) {
	reader := bufio.NewReader(strings.NewReader(strings.Repeat("x", MaxFrameBytes+10)))
	if _, err := ReadFrame(reader); err == nil {
		t.Fatal("an oversized frame was accepted")
	}
}

func TestStreamLostIsExplicit(t *testing.T) {
	err := errors.Join(errors.New("connection reset"), ErrStreamLost)
	if !IsStreamLost(err) {
		t.Fatal("a wrapped stream loss is not detected")
	}
	if IsStreamLost(errors.New("other")) {
		t.Fatal("an unrelated error was reported as a stream loss")
	}
}
