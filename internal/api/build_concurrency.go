package api

import (
	"context"

	"github.com/mbvlabs/eregion/internal/contract"
)

// BuildConcurrencyInput is a node-local operator setting, not operation input.
type BuildConcurrencyInput struct {
	Value  int  `json:"value"`
	DryRun bool `json:"dry_run"`
}

type BuildConcurrencyResult struct {
	Node      string   `json:"node"`
	Previous  int      `json:"previous"`
	Effective int      `json:"effective"`
	DryRun    bool     `json:"dry_run"`
	Warnings  []string `json:"warnings"`
}

type BuildConcurrencyService interface {
	GetBuildConcurrency(context.Context, contract.Actor) (BuildConcurrencyResult, error)
	SetBuildConcurrency(context.Context, contract.Actor, BuildConcurrencyInput) (BuildConcurrencyResult, error)
}

func (s AppService) GetBuildConcurrency(ctx context.Context, actor contract.Actor) (BuildConcurrencyResult, error) {
	if err := actor.Validate(); err != nil {
		return BuildConcurrencyResult{}, err
	}
	if actor.Kind != "operator" {
		return BuildConcurrencyResult{}, contract.NewFault(contract.Forbidden, "build concurrency requires an operator", "Use the trusted operator boundary.")
	}
	service, ok := s.Operations.(BuildConcurrencyService)
	if !ok {
		return BuildConcurrencyResult{}, contract.NewFault(contract.Unavailable, "build concurrency is unavailable", "Check node control-plane configuration.")
	}
	return service.GetBuildConcurrency(ctx, actor)
}

func (s AppService) SetBuildConcurrency(ctx context.Context, actor contract.Actor, input BuildConcurrencyInput) (BuildConcurrencyResult, error) {
	if err := actor.Validate(); err != nil {
		return BuildConcurrencyResult{}, err
	}
	if actor.Kind != "operator" {
		return BuildConcurrencyResult{}, contract.NewFault(contract.Forbidden, "build concurrency requires an operator", "Use the trusted operator boundary.")
	}
	service, ok := s.Operations.(BuildConcurrencyService)
	if !ok {
		return BuildConcurrencyResult{}, contract.NewFault(contract.Unavailable, "build concurrency is unavailable", "Check node control-plane configuration.")
	}
	return service.SetBuildConcurrency(ctx, actor, input)
}
