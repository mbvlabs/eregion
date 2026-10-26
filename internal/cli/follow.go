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
	"github.com/mbvlabs/eregion/internal/host"
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

func followConfig(cmd *cobra.Command) *followIO {
	if cmd == nil || cmd.Context() == nil {
		return nil
	}
	config, _ := cmd.Context().Value(followIOKey{}).(*followIO)
	return config
}

// followSelectionFromArgs recognizes a follow-capable invocation before Cobra
// parses it. NDJSON is rejected by output.OptionsFromArgs for every ordinary
// command, so the runner needs to know that this one invocation may stream.
type followSelection struct {
	Active bool
	NDJSON bool
	JSON   bool
}

func followSelectionFromArgs(args []string, valueFlags []string) followSelection {
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
	if len(words) >= 2 && words[0] == "operations" && words[1] == "follow" {
		selection.Active = true
	}
	// logs is a streaming read like operations follow: it opts into NDJSON
	// without changing the rules for any other command.
	if len(words) >= 1 && words[0] == "logs" {
		selection.Active = true
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
	interval, _ := cmd.Flags().GetDuration("poll-interval")
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
	interval, _ := cmd.Flags().GetDuration("poll-interval")
	if interval <= 0 {
		return contract.NewFault(contract.ValidationFailed, "poll interval must be positive", "Use --poll-interval 500ms or omit it for the default.")
	}
	interactive, _ := cmd.Flags().GetBool("interactive")
	if config.mode == string(output.Plain) && interactive && (!isTerminal(config.in) || !isTerminal(config.out)) {
		return contract.NewFault(contract.Usage, "interactive viewer requires a terminal", "Use plain output, or run --interactive with stdin and stdout attached to a terminal.")
	}
	return nil
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
	summary, err := viewer.Follow(cmd.Context(), client, id, opts, nil)
	err = followError(summary, err)
	return result{data: newFollowResult(summary)}, err
}

func (a *app) followPlain(cmd *cobra.Command, client Backend, id contract.OperationID, opts viewer.FollowOptions, config *followIO, clean viewer.Cleaner) (result, error) {
	presenter := viewer.NewPlain(config.out, config.err, clean)
	summary, err := viewer.Follow(cmd.Context(), client, id, opts, presenter.Observe)
	err = followError(summary, err)
	if finishErr := presenter.Finish(summary, err, resumeCommand+" "+string(id), time.Now()); finishErr != nil && err == nil {
		err = contract.NewFault(contract.Internal, "could not write command output", "Check the output stream and retry.")
	}
	return result{data: newFollowResult(summary), streamed: true}, err
}

func (a *app) followNDJSON(cmd *cobra.Command, client Backend, id contract.OperationID, opts viewer.FollowOptions, config *followIO, clean viewer.Cleaner) (result, error) {
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
	summary, err := viewer.Follow(cmd.Context(), client, id, opts, emit)
	err = followError(summary, err)
	envelope := contract.Success(cmd.CommandPath(), newFollowResult(summary), nil)
	if err != nil {
		envelope = contract.Failure(cmd.CommandPath(), err)
	}
	if finishErr := stream.Finish(envelope); finishErr != nil && err == nil {
		err = contract.NewFault(contract.Internal, "could not write command output", "Check the output stream and retry.")
	}
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
		return viewer.Follow(ctx, client, id, opts, emit)
	}
	summary, err := tui.Run(cmd.Context(), initial, tui.Options{
		Host:          clean(apiEndpoint(cmd)),
		ASCII:         ascii,
		ReducedMotion: reduced,
		NoColor:       os.Getenv("NO_COLOR") != "",
		In:            config.in,
		Out:           config.out,
	}, producer)
	err = followError(summary, err)
	// Bubble Tea restores the terminal on exit. Leave a stable, copyable summary.
	presenter := viewer.NewPlain(config.out, config.err, clean)
	if finishErr := presenter.Finish(summary, err, resumeCommand+" "+string(id), time.Now()); finishErr != nil && err == nil {
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

// followError turns a detach into a contract fault with identity and a resume
// command. The operation is never cancelled by leaving the viewer.
func followError(summary viewer.Summary, err error) error {
	if err == nil {
		return nil
	}
	if summary.Detached || errors.Is(err, context.Canceled) {
		return &contract.Fault{
			Code:    contract.Cancelled,
			Message: "viewer detached; the operation was not cancelled",
			Hint:    "Resume with " + resumeCommand + " " + string(summary.OperationID) + ".",
			Details: viewer.FaultDetails(summary, resumeCommand),
			Cause:   err,
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
	store, err := host.LoadContextStore("")
	if err != nil {
		return ""
	}
	context, err := store.Get("")
	if err != nil {
		return ""
	}
	return "ssh " + context.SSH.User + "@" + context.SSH.Address
}

type terminalFile interface {
	Fd() uintptr
}

func isTerminal(value any) bool {
	file, ok := value.(terminalFile)
	return ok && term.IsTerminal(file.Fd())
}
