package main

import (
	"context"
	"errors"
	"testing"
)

type fakePointers struct {
	reads  []pointer
	writes []string
	errs   []error
}

func (s *fakePointers) read(context.Context) (pointer, error) {
	p := s.reads[0]
	if len(s.reads) > 1 {
		s.reads = s.reads[1:]
	}
	return p, nil
}

func (s *fakePointers) write(_ context.Context, etag string) error {
	s.writes = append(s.writes, etag)
	if len(s.errs) == 0 {
		return nil
	}
	err := s.errs[0]
	s.errs = s.errs[1:]
	return err
}

func TestAdvanceRereadsAndReevaluates(t *testing.T) {
	store := &fakePointers{
		reads: []pointer{{etag: "old", version: "1.0.0"}, {etag: "new", version: "2.0.0"}},
		errs:  []error{errConflict},
	}
	moved, err := advance(context.Background(), store, func(p pointer) (bool, error) {
		return p.version == "1.0.0", nil
	})
	if err != nil || moved || len(store.writes) != 1 || store.writes[0] != "old" {
		t.Fatalf("moved=%v err=%v writes=%v", moved, err, store.writes)
	}
}

func TestAdvanceBoundedAndNonConflictErrors(t *testing.T) {
	store := &fakePointers{reads: []pointer{{}}, errs: []error{errConflict, errConflict, errConflict, errConflict, errConflict}}
	_, err := advance(context.Background(), store, func(pointer) (bool, error) { return true, nil })
	if err == nil || len(store.writes) != 5 || store.writes[0] != "" {
		t.Fatalf("err=%v writes=%v", err, store.writes)
	}
	failure := errors.New("access denied")
	store = &fakePointers{reads: []pointer{{etag: "x"}}, errs: []error{failure}}
	_, err = advance(context.Background(), store, func(pointer) (bool, error) { return true, nil })
	if !errors.Is(err, failure) || len(store.writes) != 1 {
		t.Fatalf("err=%v writes=%v", err, store.writes)
	}
}

func TestEdgePromotionOrdering(t *testing.T) {
	ancestor := func(a, b string) (bool, error) { return a == "a" && b == "b", nil }
	tests := []struct {
		current, candidate pointer
		want               bool
		fail               bool
	}{
		{pointer{}, pointer{version: "edge-0123456789ab", commit: "a"}, true, false},
		{pointer{version: "1.0.0-beta.1", commit: "a", etag: "x"}, pointer{version: "edge-0123456789ab", commit: "b"}, true, false},
		{pointer{version: "edge-0123456789ab", commit: "b", etag: "x"}, pointer{version: "1.0.0-beta.1", commit: "a"}, false, false},
		{pointer{version: "edge-0123456789ab", commit: "a", etag: "x"}, pointer{version: "1.0.0-alpha.1", commit: "a"}, true, false},
		{pointer{version: "1.0.0-beta.1", commit: "a", etag: "x"}, pointer{version: "edge-0123456789ab", commit: "a"}, false, false},
		{pointer{version: "1.0.0-alpha.2", commit: "a", etag: "x"}, pointer{version: "1.0.0-beta.1", commit: "a"}, true, false},
		{pointer{version: "edge-0123456789ab", commit: "a", etag: "x"}, pointer{version: "edge-abcdef012345", commit: "c"}, false, true},
	}
	for _, tt := range tests {
		got, err := edgePromotes(tt.current, tt.candidate, ancestor)
		if got != tt.want || (err != nil) != tt.fail {
			t.Errorf("%+v -> %+v: got %v, %v", tt.current, tt.candidate, got, err)
		}
	}
}
