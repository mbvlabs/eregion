package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/buildinfo"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/rpc"
	"github.com/mbvlabs/eregion/internal/viewer"
)

type rpcFollowParams struct {
	OperationID contract.OperationID `json:"operation_id"`
	After       uint64               `json:"after,string"`
	Limit       int                  `json:"limit"`
	IntervalNS  *int64               `json:"poll_interval_ns,omitempty"`
}

func validateFollowRPC(request rpc.Request) (rpcFollowParams, error) {
	var params rpcFollowParams
	if request.Stream || request.DryRun || request.Confirm != "" || request.IdempotencyKey != "" || len(request.Secrets) != 0 {
		return params, contract.NewFault(contract.ValidationFailed, "follow accepts no mutation fields or secrets", "Send a read-only follow request.")
	}
	decoder := json.NewDecoder(bytes.NewReader(request.Params))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&params); err != nil || decoder.Decode(new(any)) != io.EOF {
		return params, contract.NewFault(contract.ValidationFailed, "invalid follow parameters", "Provide operation_id, after, limit and optional poll_interval_ns.")
	}
	if err := params.OperationID.Validate(); err != nil {
		return params, err
	}
	if params.Limit == 0 {
		params.Limit = 200
	}
	if params.Limit < 1 || params.Limit > 200 || (params.IntervalNS != nil && *params.IntervalNS <= 0) {
		return params, contract.NewFault(contract.ValidationFailed, "invalid follow limit or interval", "Use a limit from 1 to 200 and a positive poll interval.")
	}
	return params, nil
}

func serveOperationFollow(ctx context.Context, app api.AppService, request rpc.Request, out io.Writer) int {
	params, err := validateFollowRPC(request)
	if err != nil {
		return writeRPCResult(out, nil, err)
	}
	actor, err := nodeOperator(request)
	if err != nil {
		return writeRPCResult(out, nil, err)
	}
	var interval time.Duration
	if params.IntervalNS != nil {
		interval = time.Duration(*params.IntervalNS)
	}
	result, err := app.FollowOperation(ctx, operations.OperatorAuthority(actor), contract.Cursor{OperationID: params.OperationID, After: params.After}, params.Limit, interval, func(observation api.OperationFollowObservation) error {
		if observation.Snapshot != nil {
			return writeFollowFrame(out, rpc.FollowPayload{Kind: "snapshot", OperationID: params.OperationID, Snapshot: observation.Snapshot, Cursor: observation.Cursor})
		}
		return writeFollowEvents(out, params.OperationID, observation.Events)
	})
	if err == nil {
		code, written, resultErr := writeFollowResult(out, result)
		if written {
			return code
		}
		err = resultErr
	}
	return writeRPCResult(out, nil, err)
}

// writeFollowEvents splits the durable event page into the largest frames that
// fit without dropping or truncating any event. Each fragment carries its own
// ending cursor.
func writeFollowEvents(out io.Writer, operationID contract.OperationID, events []contract.Event) error {
	for len(events) > 0 {
		size, err := followEventPrefixSize(operationID, events)
		if err != nil {
			return err
		}
		if size == 0 {
			return followFrameSizeError()
		}
		frame := rpc.FollowPayload{Kind: "events", OperationID: operationID, Events: events[:size], Cursor: contract.Cursor{OperationID: operationID, After: events[size-1].Sequence}}
		if err := writeFollowFrame(out, frame); err != nil {
			return err
		}
		events = events[size:]
	}
	return nil
}

func followEventPrefixSize(operationID contract.OperationID, events []contract.Event) (int, error) {
	lo, hi := 0, len(events)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		frame := rpc.FollowPayload{Kind: "events", OperationID: operationID, Events: events[:mid], Cursor: contract.Cursor{OperationID: operationID, After: events[mid-1].Sequence}}
		raw, err := rpc.MarshalEvent(rpc.EventBody{Kind: "operation_follow", Follow: &frame})
		if err != nil {
			return 0, err
		}
		if len(raw) <= rpc.MaxFollowFrameBytes {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo, nil
}

func writeFollowResult(out io.Writer, result api.OperationFollowResult) (int, bool, error) {
	data, err := json.Marshal(result)
	if err != nil {
		return 0, false, err
	}
	line, err := rpc.MarshalResult(rpc.Result{OK: true, Data: data})
	if err != nil || len(line) > rpc.MaxFollowFrameBytes {
		return 0, false, followFrameSizeError()
	}
	n, err := out.Write(line)
	if err != nil {
		return contract.ExitCode(err), true, nil
	}
	if n != len(line) {
		return contract.ExitCode(io.ErrShortWrite), true, nil
	}
	return 0, true, nil
}

func writeFollowFrame(out io.Writer, payload rpc.FollowPayload) error {
	line, err := rpc.MarshalEvent(rpc.EventBody{Kind: "operation_follow", Follow: &payload})
	if err != nil {
		return err
	}
	if len(line) > rpc.MaxFollowFrameBytes {
		return followFrameSizeError()
	}
	n, err := out.Write(line)
	if err == nil && n != len(line) {
		return io.ErrShortWrite
	}
	return err
}

func followFrameSizeError() error {
	return contract.NewFault(contract.ValidationFailed, "operation follow frame exceeds 4 MiB", "Inspect the operation without streaming and report the oversized record.")
}

func (n *nodeClient) FollowSession(ctx context.Context, cursor contract.Cursor, limit int, interval time.Duration, emit func(viewer.SessionObservation) error) (viewer.SessionResult, error) {
	if err := n.ensureVersion(ctx); err != nil {
		return viewer.SessionResult{}, err
	}
	ns := int64(interval)
	params, err := json.Marshal(rpcFollowParams{OperationID: cursor.OperationID, After: cursor.After, Limit: limit, IntervalNS: &ns})
	if err != nil {
		return viewer.SessionResult{}, err
	}
	request := rpc.Request{Schema: rpc.Schema, RequestID: rpc.NewRequestID(), Method: methodFollow, Params: params,
		Operator: n.operator, ClientVersion: buildinfo.Version, ClientCommit: buildinfo.Commit}
	payload, err := json.Marshal(request)
	if err != nil {
		return viewer.SessionResult{}, err
	}
	payload = append(payload, '\n')
	sessionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	runErr := make(chan error, 1)
	go func() {
		err := n.remote.Run(sessionCtx, nodeRPCCommand(n.operator), bytes.NewReader(payload), pw, io.Discard)
		_ = pw.CloseWithError(err)
		runErr <- err
	}()
	result, parseErr := readFollowSession(pr, cursor.OperationID, emit)
	// Stop the producer before waiting: callback/parser errors can otherwise
	// leave the remote blocked forever writing to the pipe.
	cancel()
	_ = pr.Close()
	transportErr := <-runErr
	if parseErr != nil {
		return viewer.SessionResult{}, parseErr
	}
	if ctx.Err() != nil {
		return viewer.SessionResult{}, ctx.Err()
	}
	if transportErr != nil {
		return viewer.SessionResult{}, contract.NewFault(contract.Unavailable, "operation follow connection ended", "Reconnect from the last processed event.")
	}
	return result, nil
}

type followEnvelope struct {
	Schema string          `json:"schema"`
	Type   string          `json:"type"`
	Event  *rpc.EventBody  `json:"event,omitempty"`
	OK     *bool           `json:"ok,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
	Error  *contract.Fault `json:"error,omitempty"`
}

func readFollowSession(input io.Reader, id contract.OperationID, emit func(viewer.SessionObservation) error) (viewer.SessionResult, error) {
	reader := bufio.NewReaderSize(input, 64*1024)
	for {
		line, err := readFollowLine(reader)
		if err != nil {
			return viewer.SessionResult{}, followReadError(err)
		}
		envelope, err := decodeFollowEnvelope(line)
		if err != nil {
			return viewer.SessionResult{}, err
		}
		switch envelope.Type {
		case "event":
			if err := emitFollowEvent(envelope, id, emit); err != nil {
				return viewer.SessionResult{}, err
			}
		case "result":
			return readFollowResult(reader, envelope)
		default:
			return viewer.SessionResult{}, followStreamProtocolError()
		}
	}
}

func followReadError(err error) error {
	var fault *contract.Fault
	if errors.As(err, &fault) {
		return fault
	}
	return contract.NewFault(contract.Unavailable, "operation follow ended without a result", "Reconnect from the last processed event.")
}

func decodeFollowEnvelope(line []byte) (followEnvelope, error) {
	var envelope followEnvelope
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&envelope) != nil || decoder.Decode(new(any)) != io.EOF || envelope.Schema != rpc.Schema {
		return envelope, followStreamProtocolError()
	}
	return envelope, nil
}

func emitFollowEvent(envelope followEnvelope, id contract.OperationID, emit func(viewer.SessionObservation) error) error {
	if envelope.OK != nil || len(envelope.Data) != 0 || envelope.Error != nil || envelope.Event == nil ||
		envelope.Event.Kind != "operation_follow" || envelope.Event.Follow == nil ||
		envelope.Event.Step != "" || envelope.Event.Message != "" || envelope.Event.Log != nil {
		return followStreamProtocolError()
	}
	frame := envelope.Event.Follow
	if frame.OperationID != id || frame.Cursor.OperationID != id {
		return followStreamProtocolError()
	}
	if err := validateFollowFrame(frame); err != nil {
		return err
	}
	return emit(viewer.SessionObservation{Snapshot: frame.Snapshot, Events: frame.Events, Cursor: frame.Cursor})
}

func validateFollowFrame(frame *rpc.FollowPayload) error {
	switch frame.Kind {
	case "snapshot":
		if frame.Snapshot == nil || frame.Events != nil {
			return followStreamProtocolError()
		}
	case "events":
		if frame.Snapshot != nil || len(frame.Events) == 0 {
			return followStreamProtocolError()
		}
	default:
		return followStreamProtocolError()
	}
	return nil
}

func readFollowResult(reader *bufio.Reader, envelope followEnvelope) (viewer.SessionResult, error) {
	if envelope.Event != nil || envelope.OK == nil {
		return viewer.SessionResult{}, followStreamProtocolError()
	}
	if !*envelope.OK {
		if envelope.Error == nil || !contract.KnownCode(envelope.Error.Code) || len(envelope.Data) != 0 {
			return viewer.SessionResult{}, followStreamProtocolError()
		}
		// A failed RPC exits nonzero, so the transport may close the
		// pipe with the remote exit error rather than io.EOF.
		if _, err := reader.ReadByte(); err == nil {
			return viewer.SessionResult{}, followStreamProtocolError()
		}
		return viewer.SessionResult{}, envelope.Error
	}
	if envelope.Error != nil || len(envelope.Data) == 0 {
		return viewer.SessionResult{}, followStreamProtocolError()
	}
	var result api.OperationFollowResult
	dec := json.NewDecoder(bytes.NewReader(envelope.Data))
	dec.DisallowUnknownFields()
	if dec.Decode(&result) != nil || dec.Decode(new(any)) != io.EOF {
		return viewer.SessionResult{}, followStreamProtocolError()
	}
	if _, err := reader.ReadByte(); !errors.Is(err, io.EOF) {
		return viewer.SessionResult{}, followStreamProtocolError()
	}
	return viewer.SessionResult{Operation: result.Operation, Cursor: result.Cursor}, nil
}

func readFollowLine(reader *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		part, err := reader.ReadSlice('\n')
		if len(line)+len(part) > rpc.MaxFollowFrameBytes {
			return nil, followFrameSizeError()
		}
		line = append(line, part...)
		if err == nil {
			return line, nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) && len(line) > 0 {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
}

func followStreamProtocolError() error {
	return contract.NewFault(contract.ValidationFailed, "invalid operation follow stream", "Check operator and node compatibility; do not assume the operation stopped.")
}
