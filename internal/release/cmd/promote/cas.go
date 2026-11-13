package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/mbvlabs/eregion/internal/release"
)

// errConflict means a conditional write lost a race, not a general storage error.
var errConflict = errors.New("pointer changed")

// pointer is already signature-verified and its commit is resolved from Git,
// not from manifest fields. An empty ETag denotes an absent object.
type pointer struct {
	version string
	commit  string
	etag    string
}

type pointerStore interface {
	read(context.Context) (pointer, error)
	write(context.Context, string) error
}

// advance repeats the entire read and policy decision after each CAS conflict.
// The store must use If-None-Match: * for an empty ETag and If-Match otherwise.
func advance(ctx context.Context, store pointerStore, decide func(pointer) (bool, error)) (bool, error) {
	const maxAttempts = 5
	for range maxAttempts {
		current, err := store.read(ctx)
		if err != nil {
			return false, err
		}
		ok, err := decide(current)
		if err != nil || !ok {
			return false, err
		}
		err = store.write(ctx, current.etag)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, errConflict) {
			return false, err
		}
	}
	return false, fmt.Errorf("pointer changed during all %d promotion attempts", maxAttempts)
}

// edgePromotes orders verified Git identities before comparing release versions.
// isAncestor must distinguish divergence from an inability to prove ancestry.
func edgePromotes(current, candidate pointer, isAncestor func(string, string) (bool, error)) (bool, error) {
	channel, valid := release.VersionChannel(candidate.version)
	if !valid || channel != release.ChannelEdge || candidate.commit == "" {
		return false, fmt.Errorf("invalid edge candidate")
	}
	if current.etag == "" {
		return true, nil
	}
	channel, valid = release.VersionChannel(current.version)
	if !valid || channel != release.ChannelEdge || current.commit == "" {
		return false, fmt.Errorf("invalid current edge pointer")
	}
	if current.commit != candidate.commit {
		forward, err := isAncestor(current.commit, candidate.commit)
		if err != nil {
			return false, err
		}
		backward, err := isAncestor(candidate.commit, current.commit)
		if err != nil {
			return false, err
		}
		switch {
		case forward && !backward:
			return true, nil
		case backward && !forward:
			return false, nil
		default:
			return false, fmt.Errorf("edge commits are divergent or ambiguous")
		}
	}
	a, aTagged := release.ParseStable(current.version)
	b, bTagged := release.ParseStable(candidate.version)
	switch {
	case !aTagged && bTagged:
		return true, nil
	case aTagged && !bTagged:
		return false, nil
	case !aTagged && !bTagged:
		return false, nil
	default:
		return release.CompareStable(b, a) > 0, nil
	}
}
