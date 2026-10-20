package tui

import (
	"context"
	"errors"
	"sync"

	tea "charm.land/bubbletea/v2"
	"github.com/mbvlabs/eregion/internal/viewer"
)

// Run starts the interactive viewer and follows the operation through producer.
// The program exits when the operation reaches a terminal state or when the
// person detaches. Detaching cancels only this viewer's follow context.
func Run(ctx context.Context, initial *viewer.Model, opts Options, producer Producer) (viewer.Summary, error) {
	opts = opts.normalized()
	if initial == nil {
		return viewer.Summary{}, errors.New("tui: initial viewer model is required")
	}
	programCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	model := newModel(*initial, opts)
	program := tea.NewProgram(
		model,
		tea.WithContext(programCtx),
		tea.WithInput(opts.In),
		tea.WithOutput(opts.Out),
		tea.WithWindowSize(opts.Width, opts.Height),
	)

	var (
		mu          sync.Mutex
		summary     viewer.Summary
		producerErr error
	)
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		result, err := producer(programCtx, func(observation viewer.Observation) error {
			program.Send(observationMsg{observation: observation})
			return nil
		})
		mu.Lock()
		summary, producerErr = result, err
		mu.Unlock()
		program.Send(doneMsg{summary: result, err: err})
	}()

	finalModel, runErr := program.Run()
	cancel()
	<-producerDone

	mu.Lock()
	result, followErr := summary, producerErr
	mu.Unlock()
	if result.OperationID == "" {
		if final, ok := finalModel.(Model); ok {
			result, followErr = final.summary, final.err
		}
	}
	if followErr == nil && result.OperationID == "" && runErr != nil && !isQuit(runErr) {
		return viewer.Summary{}, runErr
	}
	return result, followErr
}
