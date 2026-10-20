package workspaceui

import (
	"context"
	"errors"

	tea "charm.land/bubbletea/v2"
	"github.com/mbvlabs/eregion/internal/contract"
)

// Run opens the interactive workspace. It returns when the operator detaches
// (q or Ctrl+C) or the context ends. Detaching never cancels an operation.
func Run(ctx context.Context, opts Options) (Summary, error) {
	if opts.API == nil {
		return Summary{}, contract.NewFault(contract.InputRequired, "an API client is required", "Configure EREGION_API_URL and EREGION_API_TOKEN before opening the workspace.")
	}
	opts = opts.normalized()
	program := tea.NewProgram(New(opts),
		tea.WithContext(ctx),
		tea.WithInput(opts.In),
		tea.WithOutput(opts.Out),
	)
	final, err := program.Run()
	summary := Summary{}
	if model, ok := final.(Model); ok {
		summary = model.summary
		if model.detached {
			summary.Detached = true
		}
	}
	if err != nil {
		if errors.Is(err, tea.ErrProgramKilled) || errors.Is(err, tea.ErrInterrupted) || ctx.Err() != nil {
			summary.Detached = true
			return summary, nil
		}
		return summary, contract.NewFault(contract.Unavailable, "interactive workspace stopped unexpectedly", "Reopen the workspace; inventory reads and submitted operations are durable.")
	}
	if !summary.Detached {
		summary.Detached = true
	}
	if summary.Tab == "" {
		summary.Tab = opts.InitialTab.String()
	}
	return summary, nil
}
