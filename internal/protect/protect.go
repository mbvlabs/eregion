// Package protect is the node's one-time-password gate: the TOTP secret that
// mints tokens and exports handoffs. The secret lives root-only on the node,
// outside the database and outside every operator-visible file.
package protect

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/totp"
)

// StatePath holds the confirmed or pending TOTP secret plus replay and
// lockout state. Root only (0600).
func StatePath() string {
	return filepath.Join(host.DefaultLayout().ConfigDir, "protect.json")
}

const (
	// CodeSecret is the request secret that carries a TOTP code. As a secret
	// it is redacted from output and never written to a journal.
	CodeSecret = "protect_code"
	// Issuer names eregion in the authenticator app.
	Issuer = "eregion"

	maxFailures = 5
	lockout     = 15 * time.Minute
)

// State is the persisted gate state.
type State struct {
	// Secret is the confirmed base32 secret; empty until Confirm.
	Secret string `json:"secret,omitempty"`
	// Pending is a secret Init made that is not confirmed yet.
	Pending     string    `json:"pending,omitempty"`
	ConfirmedAt time.Time `json:"confirmed_at,omitzero"`
	// LastStep is the time step of the last accepted code for Secret; a code
	// for it or an earlier step is refused.
	LastStep    int64     `json:"last_step,omitempty"`
	Failures    int       `json:"failures,omitempty"`
	LockedUntil time.Time `json:"locked_until,omitzero"`
}

// Status is the non-secret protect.status result.
type Status struct {
	Configured  bool   `json:"configured"`
	Pending     bool   `json:"pending"`
	ConfirmedAt string `json:"confirmed_at,omitempty"`
	Failures    int    `json:"failures"`
	LockedUntil string `json:"locked_until,omitempty"`
}

// Setup is the one-time protect.init result. The secret is shown once and is
// never part of a revision, operation or journal.
type Setup struct {
	Secret string `json:"secret"`
	URI    string `json:"uri"`
	Rotate bool   `json:"rotate"`
}

// Store reads and writes the gate state. Tests point it at a temp file and a
// fixed clock.
type Store struct {
	Path string
	Now  func() time.Time
}

// NewStore is the production store at StatePath.
func NewStore() *Store {
	return &Store{Path: StatePath(), Now: time.Now}
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Store) load() (State, error) {
	var st State
	info, err := os.Lstat(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return st, contract.NewFault(contract.PreconditionFailed, "the node's two-factor state is not root-only", "Set "+s.Path+" to a regular 0600 file owned by root.")
	}
	raw, err := os.ReadFile(s.Path) //nolint:gosec // G304: the fixed root-only gate path.
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(raw, &st); err != nil {
		return st, contract.NewFault(contract.PreconditionFailed, "the node's two-factor state does not parse", "Inspect "+s.Path+" as root; do not replace it blindly.")
	}
	return st, nil
}

func (s *Store) save(st State) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return contract.NewFault(contract.Unavailable, "could not create the two-factor state directory", err.Error())
	}
	temporary, err := os.CreateTemp(dir, ".protect-")
	if err != nil {
		return contract.NewFault(contract.Unavailable, "could not write the two-factor state", err.Error())
	}
	name := temporary.Name()
	defer func() { _ = os.Remove(name) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return contract.NewFault(contract.Unavailable, "could not protect the two-factor state", err.Error())
	}
	if _, err := temporary.Write(raw); err != nil {
		_ = temporary.Close()
		return contract.NewFault(contract.Unavailable, "could not write the two-factor state", err.Error())
	}
	if err := temporary.Close(); err != nil {
		return contract.NewFault(contract.Unavailable, "could not finish the two-factor state", err.Error())
	}
	if err := os.Rename(name, s.Path); err != nil {
		return contract.NewFault(contract.Unavailable, "could not replace the two-factor state", err.Error())
	}
	return nil
}

// Status reports the gate without accepting a code.
func (s *Store) Status() (Status, error) {
	st, err := s.load()
	if err != nil {
		return Status{}, err
	}
	out := Status{Configured: st.Secret != "", Pending: st.Pending != "", Failures: st.Failures}
	if !st.ConfirmedAt.IsZero() {
		out.ConfirmedAt = st.ConfirmedAt.UTC().Format(time.RFC3339)
	}
	if s.now().Before(st.LockedUntil) {
		out.LockedUntil = st.LockedUntil.UTC().Format(time.RFC3339)
	}
	return out, nil
}

// Init makes a new pending secret. With a confirmed secret it refuses unless
// rotate, and rotating needs a current code from the confirmed one.
func (s *Store) Init(rotate bool, label, code string) (Setup, error) {
	st, err := s.load()
	if err != nil {
		return Setup{}, err
	}
	switch {
	case st.Secret != "" && !rotate:
		return Setup{}, contract.NewFault(contract.PreconditionFailed, "two-factor protection is already set up on this node", "Run eregion protect init --rotate to replace it.")
	case st.Secret == "" && rotate:
		return Setup{}, contract.NewFault(contract.PreconditionFailed, "two-factor protection is not set up, so there is nothing to rotate", "Run eregion protect init without --rotate.")
	case rotate:
		if st, err = s.verify(st, code, false); err != nil {
			return Setup{}, err
		}
	}
	secret, err := totp.NewSecret()
	if err != nil {
		return Setup{}, err
	}
	st.Pending = secret
	if err := s.save(st); err != nil {
		return Setup{}, err
	}
	label = strings.TrimSpace(label)
	if label == "" {
		label, _ = os.Hostname()
	}
	return Setup{Secret: secret, URI: totp.URI(Issuer, label, secret), Rotate: rotate}, nil
}

// Confirm checks a code from the pending secret and makes it the confirmed
// one.
func (s *Store) Confirm(code string) (Status, error) {
	st, err := s.load()
	if err != nil {
		return Status{}, err
	}
	if st.Pending == "" {
		return Status{}, contract.NewFault(contract.PreconditionFailed, "there is no pending two-factor setup to confirm", "Run eregion protect init first.")
	}
	st, err = s.verify(st, code, true)
	if err != nil {
		return Status{}, err
	}
	return s.Status()
}

// Verify checks a current code against the confirmed secret without changing
// the setup. The accepted step is recorded, so a code works once.
func (s *Store) Verify(code string) error {
	st, err := s.load()
	if err != nil {
		return err
	}
	if st.Secret == "" {
		return contract.NewFault(contract.PreconditionFailed, "this node has no two-factor protection set up", "A human runs eregion protect init at a terminal first.")
	}
	_, err = s.verify(st, code, false)
	return err
}

func (s *Store) verify(st State, code string, pending bool) (State, error) {
	now := s.now()
	if now.Before(st.LockedUntil) {
		return st, lockedFault(st.LockedUntil)
	}
	secret, last := st.Secret, st.LastStep
	if pending {
		// A pending secret has never accepted a code.
		secret, last = st.Pending, 0
	}
	if strings.TrimSpace(code) == "" {
		return st, codeFault("a code from the authenticator app is required", "code_required", "Run the command yourself at a terminal; it asks for the current 6-digit code.")
	}
	key, err := totp.DecodeSecret(secret)
	if err != nil {
		return st, contract.NewFault(contract.PreconditionFailed, "the node's two-factor secret is not readable", "Re-run eregion protect init --rotate as root.")
	}
	step, verr := totp.Verify(key, code, now, last)
	if verr != nil {
		st.Failures++
		left := maxFailures - st.Failures
		if st.Failures >= maxFailures {
			st.Failures, st.LockedUntil = 0, now.Add(lockout)
		}
		if err := s.save(st); err != nil {
			return st, err
		}
		if now.Before(st.LockedUntil) {
			return st, lockedFault(st.LockedUntil)
		}
		reason := "code_invalid"
		if errors.Is(verr, totp.ErrReused) {
			reason = "code_reused"
		}
		return st, codeFault(verr.Error(), reason,
			fmt.Sprintf("Wait for the next code in the authenticator app and try again. %d more wrong code(s) lock codes out for 15 minutes.", left))
	}
	st.Failures, st.LockedUntil = 0, time.Time{}
	if pending {
		st.Secret, st.Pending, st.ConfirmedAt = st.Pending, "", now.UTC()
	}
	st.LastStep = step
	return st, s.save(st)
}

func codeFault(message, reason, hint string) *contract.Fault {
	fault := contract.NewFault(contract.PreconditionFailed, message, hint)
	fault.Details = map[string]any{"reason": reason}
	return fault
}

func lockedFault(until time.Time) *contract.Fault {
	fault := codeFault("too many wrong codes; codes are locked out until "+until.UTC().Format(time.RFC3339), "locked",
		"Wait until the lockout ends, then try again with a fresh code. eregion protect status shows it.")
	fault.Details["locked_until"] = until.UTC().Format(time.RFC3339)
	return fault
}

// Retryable reports whether err is a wrong or reused code that a fresh code
// may fix; a lockout is not.
func Retryable(err error) bool {
	var fault *contract.Fault
	if !errors.As(err, &fault) {
		return false
	}
	reason, _ := fault.Details["reason"].(string)
	return reason == "code_invalid" || reason == "code_reused"
}
