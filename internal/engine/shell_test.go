//go:build unix

package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/secrets"
)

func TestShellRedactsAndKeepsTail(t *testing.T) {
	registry := &secrets.Registry{}
	registry.Add("secret-value")
	shell := Shell{Secrets: registry}
	var events []Event
	err := shell.Run(context.Background(), "shell-step-v1", "echo secret-value\necho done\nexit 3", nil, func(event Event) {
		events = append(events, event)
	})
	var scriptErr *ScriptError
	if !errors.As(err, &scriptErr) || scriptErr.Status != 3 {
		t.Fatalf("expected script error, got %v", err)
	}
	for _, event := range events {
		if strings.Contains(event.Message, "secret-value") {
			t.Fatalf("shell leaked a secret: %+v", event)
		}
	}
	if len(events) != 2 || events[0].Message != secrets.Placeholder || events[1].Message != "done" {
		t.Fatalf("unexpected events: %+v", events)
	}
	if len(scriptErr.Tail) != 2 {
		t.Fatalf("unexpected tail: %+v", scriptErr.Tail)
	}
}

func TestShellCancelsProcessGroup(t *testing.T) {
	shell := Shell{Secrets: &secrets.Registry{}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := shell.Run(ctx, "slow-step-v1", "sleep 5", nil, nil)
	if !errors.Is(err, ErrInterrupted) {
		t.Fatalf("expected interruption, got %v", err)
	}
}
