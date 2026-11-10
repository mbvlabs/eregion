package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/output"
	"github.com/mbvlabs/eregion/internal/tui"
	"github.com/mbvlabs/eregion/internal/viewer"
	"github.com/spf13/cobra"
)

// followIO carries the real invocation streams to a viewer command. Cobra is
// deliberately given io.Discard as its output so a renderer cannot accidentally
// write a second envelope; this context value is the single hand-off.
type followIO struct {
	in   io.Reader
	out  io.Writer
	err  io.Writer
	mode string
}

type followIOKey struct{}

// streamContextKey marks remote calls made by an event-following loop. The
// viewer polls repeatedly, so a loading line would mix into its stream.
type streamContextKey struct{}

func withoutWait(ctx context.Context) context.Context {
	return context.WithValue(ctx, streamContextKey{}, true)
}

func waitSuppressed(ctx context.Context) bool {
	suppressed, _ := ctx.Value(streamContextKey{}).(bool)
	return suppressed
}

func followConfig(cmd *cobra.Command) *followIO {
	if cmd == nil || cmd.Context() == nil {
		return nil
	}
	config, _ := cmd.Context().Value(followIOKey{}).(*followIO)
	return config
}

// streamsNDJSON reports whether this invocation explicitly selected the
// streaming machine format. It is true only for commands whose metadata lists
// ndjson and whose arguments asked for it.
func streamsNDJSON(cmd *cobra.Command) bool {
	config := followConfig(cmd)
	return config != nil && config.mode == "ndjson"
}

// followPollInterval returns the command's poll interval, or the default when
// the command has no flag. An explicit non-positive value is a usage error so
// --poll-interval 0 cannot spin the viewer.
func followPollInterval(cmd *cobra.Command) (time.Duration, error) {
	if cmd == nil || cmd.Flags().Lookup("poll-interval") == nil {
		return viewer.DefaultPollInterval, nil
	}
	interval, _ := cmd.Flags().GetDuration("poll-interval")
	if interval <= 0 {
		return 0, contract.NewFault(contract.ValidationFailed, "poll interval must be positive", "Use --poll-interval 500ms or omit it for the default.")
	}
	return interval, nil
}

// followSelection says whether the invocation names a command that may stream
// events and whether it asked for NDJSON. The runner needs it before Cobra
// parses, because output.OptionsFromArgs rejects ndjson for ordinary commands.
type followSelection struct {
	Active bool
	NDJSON bool
	JSON   bool
}

// followingMeta marks a command that follows an accepted operation (or a log
// stream) as accepting NDJSON. Metadata is the single source of truth: the
// runner's pre-parse selection and `commands --json` both read it.
func followingMeta(meta CommandMeta) CommandMeta {
	meta.OutputModes = []string{"plain", "json", "ndjson"}
	return meta
}

func (a *app) followSelection(args []string) followSelection {
	return followSelectionFor(a.root, a.meta, args, a.valueFlags())
}

func followSelectionFor(root *cobra.Command, metadata registry, args []string, valueFlags []string) followSelection {
	var words []string
	var selected string
	var outputSet, jsonEnabled bool
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		if strings.HasPrefix(arg, "--") {
			name, value, hasValue := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
			switch name {
			case "output":
				outputSet = true
				if hasValue {
					selected = value
				} else if i+1 < len(args) {
					i++
					selected = args[i]
				}
			case "json":
				switch {
				case !hasValue:
					jsonEnabled = true
				case value == "true":
					jsonEnabled = true
				case value == "false":
					jsonEnabled = false
				}
			default:
				if !hasValue {
					for _, flag := range valueFlags {
						if name == flag && i+1 < len(args) {
							i++ // A value that resembles a command word is still a value.
							break
						}
					}
				}
			}
			continue
		}
		if strings.HasPrefix(arg, "-") {
			continue
		}
		words = append(words, arg)
	}
	selection := followSelection{JSON: jsonEnabled}
	if len(words) > 0 {
		if target, _, err := root.Find(words); err == nil {
			for _, mode := range metadata[target].OutputModes {
				if mode == "ndjson" {
					selection.Active = true
					break
				}
			}
		}
	}
	if selection.Active && outputSet && selected == "ndjson" {
		selection.NDJSON = true
	}
	return selection
}

// followFlags configures an operation follow command.
func followFlags(cmd *cobra.Command) {
	cmd.Flags().Bool("interactive", false, "Start the interactive terminal viewer (requires a terminal; ignored by machine output)")
	cmd.Flags().Bool("ascii", false, "Use ASCII glyphs in the interactive viewer")
	cmd.Flags().Bool("reduced-motion", false, "Replace animation with static state labels")
	cmd.Flags().Duration("poll-interval", viewer.DefaultPollInterval, "Snapshot and event poll interval")
}

const resumeCommand = "eregion operations follow"

func (a *app) follow(cmd *cobra.Command, args []string) (result, error) {
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	return a.followOperation(cmd, client, contract.OperationID(args[0]))
}

// followOperation runs the shared follow loop through the selected presenter.
// It never submits, cancels, or executes work.
func (a *app) followOperation(cmd *cobra.Command, client Backend, id contract.OperationID) (result, error) {
	if err := validateFollow(cmd); err != nil {
		return result{}, err
	}
	config := followConfig(cmd)
	interval, _ := followPollInterval(cmd)
	clean := a.followCleaner()
	opts := viewer.FollowOptions{PollInterval: interval, Clean: clean}

	interactive, _ := cmd.Flags().GetBool("interactive")
	if config.mode == string(output.Plain) && interactive {
		return a.followInteractive(cmd, client, id, opts, config, clean)
	}

	switch config.mode {
	case "ndjson":
		return a.followNDJSON(cmd, client, id, opts, config, clean)
	case string(output.JSON):
		return a.followJSON(cmd, client, id, opts)
	default:
		return a.followPlain(cmd, client, id, opts, config, clean)
	}
}

// Validate before submission too: an unusable viewer must not accept new work.
func validateFollow(cmd *cobra.Command) error {
	config := followConfig(cmd)
	if config == nil {
		return contract.NewFault(contract.Internal, "viewer output is not configured", "Invoke the command through the eregion CLI entry point.")
	}
	if _, err := followPollInterval(cmd); err != nil {
		return err
	}
	interactive, _ := cmd.Flags().GetBool("interactive")
	if config.mode == string(output.Plain) && interactive && (!isTerminal(config.in) || !isTerminal(config.out)) {
		return contract.NewFault(contract.Usage, "interactive viewer requires a terminal", "Use plain output, or run --interactive with stdin and stdout attached to a terminal.")
	}
	return nil
}

// validateFollowRequest rejects unusable viewer options before a command
// accepts work. Without it, an operation could be queued for a command that
// can never display it.
func validateFollowRequest(cmd *cobra.Command, follow bool) error {
	if !follow {
		return nil
	}
	return validateFollow(cmd)
}

type followResult struct {
	Operation    contract.Operation      `json:"operation"`
	State        contract.State          `json:"state"`
	Attempt      uint32                  `json:"attempt"`
	ErrorCode    contract.Code           `json:"error_code,omitempty"`
	Events       uint64                  `json:"events"`
	Cursor       uint64                  `json:"cursor,string"`
	LastSequence uint64                  `json:"last_sequence,string"`
	Observation  viewer.ObservationState `json:"observation"`
	Detached     bool                    `json:"detached"`
}

func newFollowResult(summary viewer.Summary) followResult {
	return followResult{
		Operation:    summary.Operation,
		State:        summary.State,
		Attempt:      summary.Attempt,
		ErrorCode:    summary.ErrorCode,
		Events:       summary.Events,
		Cursor:       summary.Cursor,
		LastSequence: summary.LastSequence,
		Observation:  summary.Observation,
		Detached:     summary.Detached,
	}
}

func (a *app) followCleaner() viewer.Cleaner {
	registry := a.secrets
	return func(text string) string {
		if registry == nil {
			return output.SafeText(text)
		}
		return output.SafeText(registry.Redact(text))
	}
}

func (a *app) followJSON(cmd *cobra.Command, client Backend, id contract.OperationID, opts viewer.FollowOptions) (result, error) {
	summary, err := viewer.Follow(withoutWait(cmd.Context()), client, id, opts, nil)
	err = followError(summary, err)
	return result{data: newFollowResult(summary)}, err
}

func (a *app) followPlain(cmd *cobra.Command, client Backend, id contract.OperationID, opts viewer.FollowOptions, config *followIO, clean viewer.Cleaner) (result, error) {
	presenter := viewer.NewPlain(config.out, config.err, clean)
	summary, err := viewer.Follow(withoutWait(cmd.Context()), client, id, opts, presenter.Observe)
	observedOK := err == nil
	err = followError(summary, err)
	presentationErr := err
	if observedOK {
		presentationErr = nil // Keep the terminal outcome card for an observed failure.
	}
	if finishErr := presenter.Finish(summary, presentationErr, resumeCommand+" "+string(id), time.Now()); finishErr != nil && err == nil {
		err = contract.NewFault(contract.Internal, "could not write command output", "Check the output stream and retry.")
	}
	return result{data: newFollowResult(summary), streamed: true}, err
}

// followNDJSONSummary streams the operation's events and returns the still-open
// stream with the terminal summary. The caller decides when to write the one
// final record, so a guided create can continue into its first deploy without
// emitting two terminal results.
func (a *app) followNDJSONSummary(cmd *cobra.Command, client Backend, id contract.OperationID, opts viewer.FollowOptions, config *followIO, clean viewer.Cleaner) (*output.Stream, viewer.Summary, error) {
	stream := output.NewStream(config.out, a.secrets)
	var last uint64
	emit := func(observation viewer.Observation) error {
		if observation.Kind != viewer.ObservationEvents {
			return nil
		}
		for _, event := range observation.Events {
			if event.Sequence <= last {
				continue // Defensive: never re-emit a replay or duplicate.
			}
			cleanEvent(&event, clean)
			if err := stream.Event(event); err != nil {
				return err
			}
			last = event.Sequence
		}
		return nil
	}
	summary, err := viewer.Follow(withoutWait(cmd.Context()), client, id, opts, emit)
	return stream, summary, followError(summary, err)
}

// finishNDJSON writes the single terminal result record for a streamed follow.
func finishNDJSON(stream *output.Stream, cmd *cobra.Command, summary viewer.Summary, err error) error {
	envelope := contract.Success(cmd.CommandPath(), newFollowResult(summary), nil)
	if err != nil {
		envelope = contract.Failure(cmd.CommandPath(), err)
	}
	if finishErr := stream.Finish(envelope); finishErr != nil && err == nil {
		return contract.NewFault(contract.Internal, "could not write command output", "Check the output stream and retry.")
	}
	return err
}

func (a *app) followNDJSON(cmd *cobra.Command, client Backend, id contract.OperationID, opts viewer.FollowOptions, config *followIO, clean viewer.Cleaner) (result, error) {
	stream, summary, err := a.followNDJSONSummary(cmd, client, id, opts, config, clean)
	err = finishNDJSON(stream, cmd, summary, err)
	return result{data: newFollowResult(summary), streamed: true}, err
}

func (a *app) followInteractive(cmd *cobra.Command, client Backend, id contract.OperationID, opts viewer.FollowOptions, config *followIO, clean viewer.Cleaner) (result, error) {
	ascii, _ := cmd.Flags().GetBool("ascii")
	reduced, _ := cmd.Flags().GetBool("reduced-motion")
	if strings.EqualFold(os.Getenv("TERM"), "dumb") {
		ascii = true
	}
	if os.Getenv("EREGION_REDUCED_MOTION") == "1" {
		reduced = true
	}
	initial := viewer.New(id, viewer.Options{Clean: clean})
	producer := func(ctx context.Context, emit func(viewer.Observation) error) (viewer.Summary, error) {
		return viewer.Follow(withoutWait(ctx), client, id, opts, emit)
	}
	summary, err := tui.Run(cmd.Context(), initial, tui.Options{
		Host:          clean(apiEndpoint(cmd)),
		ASCII:         ascii,
		ReducedMotion: reduced,
		NoColor:       os.Getenv("NO_COLOR") != "",
		In:            config.in,
		Out:           config.out,
	}, producer)
	observedOK := err == nil
	err = followError(summary, err)
	presentationErr := err
	if observedOK {
		presentationErr = nil
	}
	// Bubble Tea restores the terminal on exit. Leave a stable, copyable summary.
	presenter := viewer.NewPlain(config.out, config.err, clean)
	if finishErr := presenter.Finish(summary, presentationErr, resumeCommand+" "+string(id), time.Now()); finishErr != nil && err == nil {
		err = contract.NewFault(contract.Internal, "could not write command output", "Check the output stream and retry.")
	}
	return result{data: newFollowResult(summary), streamed: true}, err
}

func cleanEvent(event *contract.Event, clean viewer.Cleaner) {
	event.Message = clean(event.Message)
	event.Stage = clean(event.Stage)
	event.ExecutionID = clean(event.ExecutionID)
	if event.Progress != nil {
		progress := *event.Progress
		progress.Unit = clean(progress.Unit)
		event.Progress = &progress
	}
}

// followError maps observed terminal outcomes and observation errors to CLI
// faults. A failed operation is not a successful command just because polling
// succeeded. Leaving the viewer never requests operation cancellation.
func followError(summary viewer.Summary, err error) error {
	if summary.Detached || errors.Is(err, context.Canceled) {
		return &contract.Fault{
			Code:    contract.Cancelled,
			Message: "viewer detached; the operation was not cancelled",
			Hint:    "Resume with " + resumeCommand + " " + string(summary.OperationID) + ".",
			Details: viewer.FaultDetails(summary, resumeCommand),
			Cause:   err,
		}
	}
	if err == nil {
		details := viewer.FaultDetails(summary, resumeCommand)
		details["state"] = summary.State
		if summary.ErrorCode != "" {
			details["error_code"] = summary.ErrorCode
		}
		switch summary.State {
		case contract.Failed:
			code := summary.ErrorCode
			if !contract.KnownCode(code) || code == contract.Cancelled {
				code = contract.PreconditionFailed
			}
			return &contract.Fault{
				Code: code, Message: "operation failed",
				Hint:    "Inspect operation events before submitting again.",
				Details: details,
			}
		case contract.Canceled:
			return &contract.Fault{
				Code: contract.Cancelled, Message: "operation was cancelled",
				Hint:    "Inspect operation state before submitting again.",
				Details: details,
			}
		default:
			return nil
		}
	}
	fault := contract.Classify(err)
	details := viewer.FaultDetails(summary, resumeCommand)
	for key, value := range fault.Details {
		details[key] = value
	}
	fault.Details = details
	return fault
}

func apiEndpoint(cmd *cobra.Command) string {
	current, err := selectedWorkspaceAccess(cmd)
	if err != nil || current.SSH == nil {
		return ""
	}
	return "ssh " + current.SSH.User + "@" + current.SSH.Address
}

type terminalFile interface {
	Fd() uintptr
}

func isTerminal(value any) bool {
	file, ok := value.(terminalFile)
	return ok && term.IsTerminal(file.Fd())
}
