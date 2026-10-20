package resources

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

const scheduleStateSchema = "eregion/backup-schedule/v1"

// Schedule declares when an orchestrator should execute a backup. It never
// starts a scheduler in this package. Exactly one of EverySeconds or Cron is
// required; Timezone defaults to UTC.
type Schedule struct {
	EverySeconds int64  `json:"every_seconds,omitempty"`
	Cron         string `json:"cron,omitempty"`
	Timezone     string `json:"timezone,omitempty"`
}

// Retention declares how many backup artifacts to keep and/or their maximum age.
// Zero disables that bound. At least one bound is required.
type Retention struct {
	KeepLast      int   `json:"keep_last,omitempty"`
	MaxAgeSeconds int64 `json:"max_age_seconds,omitempty"`
}

// Policy is a backup schedule and retention declaration for the orchestrator's
// River periodic job. VerifyAfter asks the verification action to run after a
// successful backup.
type Policy struct {
	Schedule    Schedule  `json:"schedule"`
	Retention   Retention `json:"retention"`
	VerifyAfter bool      `json:"verify_after,omitempty"`
}

func (p Policy) Validate() error {
	if err := p.Schedule.Validate(); err != nil {
		return err
	}
	return p.Retention.Validate()
}

func (s Schedule) Validate() error {
	if (s.EverySeconds == 0) == (strings.TrimSpace(s.Cron) == "") {
		return invalidResource("schedule requires exactly one interval or cron expression", "Set every_seconds or a five-field cron expression, not both.")
	}
	if s.EverySeconds != 0 && (s.EverySeconds < 60 || s.EverySeconds > 366*24*60*60) {
		return invalidResource("schedule interval is out of range", "Use an interval from 60 seconds to 366 days.")
	}
	if s.Cron != "" {
		if _, err := parseCron(s.Cron); err != nil {
			return err
		}
	}
	if _, err := s.location(); err != nil {
		return err
	}
	return nil
}

func (s Schedule) location() (*time.Location, error) {
	if strings.TrimSpace(s.Timezone) == "" {
		return time.UTC, nil
	}
	location, err := time.LoadLocation(s.Timezone)
	if err != nil {
		return nil, invalidResource("unknown schedule timezone", "Use an IANA timezone name or omit it for UTC.")
	}
	return location, nil
}

// Next returns the next scheduled instant strictly after the supplied time.
func (s Schedule) Next(after time.Time) (time.Time, error) {
	if err := s.Validate(); err != nil {
		return time.Time{}, err
	}
	if s.EverySeconds != 0 {
		return after.UTC().Add(time.Duration(s.EverySeconds) * time.Second), nil
	}
	location, err := s.location()
	if err != nil {
		return time.Time{}, err
	}
	expression, err := parseCron(s.Cron)
	if err != nil {
		return time.Time{}, err
	}
	return expression.next(after.In(location))
}

func (r Retention) Validate() error {
	if r.KeepLast < 0 || r.MaxAgeSeconds < 0 {
		return invalidResource("retention bounds cannot be negative", "Use zero to disable a bound or a positive count/age.")
	}
	if r.KeepLast == 0 && r.MaxAgeSeconds == 0 {
		return invalidResource("retention requires at least one bound", "Set keep_last, max_age_seconds, or both.")
	}
	if r.KeepLast > 10_000 {
		return invalidResource("retention count is too large", "Keep at most 10,000 backup artifacts.")
	}
	return nil
}

// SelectRetained returns artifacts sorted newest-first, partitioned into keep
// and prune. It is a pure declaration helper: the orchestrator owns deletion.
func (r Retention) SelectRetained(artifacts []BackupArtifact, now time.Time) (keep, prune []BackupArtifact, err error) {
	if err := r.Validate(); err != nil {
		return nil, nil, err
	}
	sorted := append([]BackupArtifact(nil), artifacts...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].CreatedAt.After(sorted[j].CreatedAt) })
	cutoff := time.Time{}
	if r.MaxAgeSeconds > 0 {
		cutoff = now.UTC().Add(-time.Duration(r.MaxAgeSeconds) * time.Second)
	}
	for index, artifact := range sorted {
		withinCount := r.KeepLast == 0 || index < r.KeepLast
		withinAge := cutoff.IsZero() || !artifact.CreatedAt.Before(cutoff)
		if withinCount && withinAge {
			keep = append(keep, artifact)
		} else {
			prune = append(prune, artifact)
		}
	}
	return keep, prune, nil
}

// cronExpression is a validated five-field expression with bitsets for each
// field. Day-of-month and day-of-week use PostgreSQL/River-like OR semantics when
// both are restricted.
type cronExpression struct {
	minute, hour, dom, month, dow uint64
	domRestricted, dowRestricted  bool
}

func parseCron(text string) (cronExpression, error) {
	fields := strings.Fields(text)
	if len(fields) != 5 {
		return cronExpression{}, invalidResource("invalid cron expression", "Use five fields: minute hour day-of-month month day-of-week.")
	}
	minute, _, err := cronField(fields[0], 0, 59)
	if err != nil {
		return cronExpression{}, err
	}
	hour, _, err := cronField(fields[1], 0, 23)
	if err != nil {
		return cronExpression{}, err
	}
	dom, domRestricted, err := cronField(fields[2], 1, 31)
	if err != nil {
		return cronExpression{}, err
	}
	month, _, err := cronField(fields[3], 1, 12)
	if err != nil {
		return cronExpression{}, err
	}
	dow, dowRestricted, err := cronField(fields[4], 0, 7)
	if err != nil {
		return cronExpression{}, err
	}
	// Normalise Sunday=7 to Sunday=0.
	if dow&(1<<7) != 0 {
		dow |= 1 << 0
		dow &^= 1 << 7
	}
	return cronExpression{
		minute: minute, hour: hour, dom: dom, month: month, dow: dow,
		domRestricted: domRestricted, dowRestricted: dowRestricted,
	}, nil
}

func cronField(text string, minimum, maximum int) (uint64, bool, error) {
	if text == "" {
		return 0, false, invalidResource("invalid cron field", "Use *, a number, a range, a list, or a step value.")
	}
	restricted := text != "*"
	var bits uint64
	for _, part := range strings.Split(text, ",") {
		step := 1
		base := part
		if slash := strings.IndexByte(part, '/'); slash >= 0 {
			base = part[:slash]
			value, err := strconv.Atoi(part[slash+1:])
			if err != nil || value < 1 || value > maximum-minimum+1 {
				return 0, false, invalidResource("invalid cron step", "Use a positive step within the field range.")
			}
			step = value
			restricted = true
		}
		start, end := minimum, maximum
		switch {
		case base == "*":
		case strings.Contains(base, "-"):
			rangeParts := strings.Split(base, "-")
			if len(rangeParts) != 2 {
				return 0, false, invalidResource("invalid cron range", "Use start-end within the field range.")
			}
			first, err1 := strconv.Atoi(rangeParts[0])
			last, err2 := strconv.Atoi(rangeParts[1])
			if err1 != nil || err2 != nil || first < minimum || last > maximum || first > last {
				return 0, false, invalidResource("invalid cron range", "Use start-end within the field range.")
			}
			start, end = first, last
		default:
			value, err := strconv.Atoi(base)
			if err != nil || value < minimum || value > maximum {
				return 0, false, invalidResource("invalid cron value", "Use a value within the field range.")
			}
			start, end = value, value
			if step != 1 {
				end = maximum
			}
		}
		for value := start; value <= end; value += step {
			bits |= 1 << uint(value)
		}
	}
	return bits, restricted, nil
}

func (c cronExpression) next(after time.Time) (time.Time, error) {
	candidate := after.Truncate(time.Minute).Add(time.Minute)
	limit := candidate.AddDate(5, 0, 0)
	for !candidate.After(limit) {
		if c.matches(candidate) {
			return candidate, nil
		}
		candidate = candidate.Add(time.Minute)
	}
	return time.Time{}, contract.NewFault(contract.ValidationFailed, "cron expression has no run time", "Use an expression that can occur within five years.")
}

func (c cronExpression) matches(at time.Time) bool {
	if c.minute&(1<<uint(at.Minute())) == 0 || c.hour&(1<<uint(at.Hour())) == 0 || c.month&(1<<uint(int(at.Month()))) == 0 {
		return false
	}
	domMatch := c.dom&(1<<uint(at.Day())) != 0
	dowMatch := c.dow&(1<<uint(int(at.Weekday()))) != 0
	if c.domRestricted && c.dowRestricted {
		return domMatch || dowMatch
	}
	return domMatch && dowMatch
}

// ScheduledBackup is the exact durable action the orchestrator should insert as
// a River periodic job. It contains no credential value.
type ScheduledBackup struct {
	Key      string   `json:"key"`
	Kind     string   `json:"kind"`
	Target   string   `json:"target"`
	Input    []byte   `json:"input,omitempty"`
	Schedule Schedule `json:"schedule"`
	Policy   Policy   `json:"policy"`
}

// PeriodicBackup validates a policy and returns the action declaration for a
// River periodic job. The caller owns River registration; this package never
// starts a scheduler or another queue.
func (s *Service) PeriodicBackup(spec Spec, policy Policy) (ScheduledBackup, error) {
	spec, err := s.normalizeSpec(spec)
	if err != nil {
		return ScheduledBackup{}, err
	}
	if err := s.guardControlPlane(spec, ActionBackup); err != nil {
		return ScheduledBackup{}, err
	}
	if err := policy.Validate(); err != nil {
		return ScheduledBackup{}, err
	}
	if _, err := s.requireBackupDir(); err != nil {
		return ScheduledBackup{}, err
	}
	request, err := EncodeRequest(Request{
		Action: ActionBackup,
		Spec:   spec,
		Backup: &BackupRequest{Reason: "scheduled", Policy: &policy},
	})
	if err != nil {
		return ScheduledBackup{}, err
	}
	return ScheduledBackup{
		Key:      "platform.backup.create/" + spec.Host + "/" + spec.Name,
		Kind:     KindBackupCreate,
		Target:   "node/" + spec.Host,
		Input:    request,
		Schedule: policy.Schedule,
		Policy:   policy,
	}, nil
}

// scheduleState is the protected, secret-free record of one applied backup
// policy. It is configuration for the orchestrator's River periodic job; this
// package still does not start a scheduler.
type scheduleState struct {
	Schema    string          `json:"schema"`
	Spec      Spec            `json:"spec"`
	Policy    Policy          `json:"policy"`
	Key       string          `json:"key"`
	Kind      string          `json:"kind"`
	Target    string          `json:"target"`
	Input     json.RawMessage `json:"input"`
	UpdatedAt time.Time       `json:"updated_at"`
}

func (s *Service) schedulePath(name string) string {
	return filepath.Join(s.resourceDir(name), "schedule.json")
}

func (s *Service) setSchedule(spec Spec, policy *Policy) (Result, error) {
	result := baseResult(ActionSchedule, spec, StateUnknown)
	if policy == nil {
		return result, invalidResource("backup policy is required", "Provide a schedule and at least one retention bound.")
	}
	scheduled, err := s.PeriodicBackup(spec, *policy)
	if err != nil {
		return result, err
	}
	state := scheduleState{
		Schema: scheduleStateSchema, Spec: spec, Policy: *policy,
		Key: scheduled.Key, Kind: scheduled.Kind, Target: scheduled.Target,
		Input: json.RawMessage(scheduled.Input), UpdatedAt: s.now(),
	}
	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return result, err
	}
	if err := ensurePrivateDir(s.resourceDir(spec.Name)); err != nil {
		return result, unavailable("could not create the protected state directory", "Check permissions on the service state directory.")
	}
	if err := writeAtomic(s.schedulePath(spec.Name), append(encoded, '\n'), 0o600); err != nil {
		return result, unavailable("could not record the backup schedule policy", "Check permissions and free space on the service state directory.")
	}
	result.Changed = true
	result.Scheduled = &ScheduledBackup{
		Key: scheduled.Key, Kind: scheduled.Kind, Target: scheduled.Target,
		Schedule: scheduled.Schedule, Policy: scheduled.Policy,
	}
	result.State = StateReady
	return result, nil
}

func formatCronError(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("schedule: %v", err)
}
