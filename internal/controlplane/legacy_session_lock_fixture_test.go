package controlplane

import "context"

// These helpers only exercise the retired lock protocol on a direct fixture
// connection. Production controlplane code does not acquire session locks.
func (s *Service) lockTarget(ctx context.Context, target string) (func(), error) {
	return s.lockTargets(ctx, []string{target})
}

func (s *Service) lockTargets(ctx context.Context, targets []string) (func(), error) {
	lease, err := s.entities.Platform().Locks().AcquireTargets(ctx, legacyTargetKeys(targets))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	return func() { _ = lease.Release() }, nil
}

func (s *Service) tryLockTargets(ctx context.Context, targets []string) (func(), bool, error) {
	lease, acquired, err := s.entities.Platform().Locks().TryTargets(ctx, legacyTargetKeys(targets))
	if err != nil || !acquired {
		return nil, false, err
	}
	return func() { _ = lease.Release() }, true, nil
}

func legacyTargetKeys(targets []string) []int64 {
	targets = uniqueTargets(targets)
	keys := make([]int64, 0, len(targets))
	for _, target := range targets {
		keys = append(keys, targetLockKey(target))
	}
	return keys
}
