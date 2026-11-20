package node

import (
	"bytes"
	"context"
	"encoding/json"
	"io"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/platform"
)

// ObserveImage handles only a typed read-only request, never an arbitrary command.
func ObserveImage(ctx context.Context, request Request) Response {
	var payload ObserveImagePayload
	decoder := json.NewDecoder(bytes.NewReader(request.Payload))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&payload) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return Response{Status: engine.StatusFailed, ErrorCode: string(contract.ValidationFailed), Detail: "invalid image observation payload"}
	}
	image, err := platform.ObserveLocalImage(ctx, payload.Reference)
	if err != nil {
		fault := contract.Classify(err)
		return Response{Status: engine.StatusFailed, ErrorCode: string(fault.Code), Detail: fault.Message}
	}
	raw, err := json.Marshal(image)
	if err != nil {
		return Response{Status: engine.StatusFailed, ErrorCode: string(contract.Internal), Detail: "image observation could not be encoded"}
	}
	return Response{Status: engine.StatusCompleted, Result: raw}
}
