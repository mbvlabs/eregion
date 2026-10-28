package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/buildinfo"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/doctor"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/rpc"
	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
)

func doctorTestInit(t *testing.T) *serverInit {
	t.Helper()
	t.Setenv("EREGION_CONTEXT_FILE", filepath.Join(t.TempDir(), "eregion", "contexts.json"))
	cmd := &cobra.Command{Use: "init"}
	cmd.SetErr(&strings.Builder{})
	return &serverInit{
		a: &app{secrets: &secrets.Registry{}}, cmd: cmd,
		pending: &host.PendingInit{Server: "prod", Phases: map[string]string{}},
	}
}

func TestDoctorGateReportsFailedChecksAndCanResume(t *testing.T) {
	r := doctorTestInit(t)
	failed := doctor.BuildReport([]doctor.Result{
		{ID: "host.disk", Category: doctor.CatHost, Status: doctor.Fail, Detail: "8% free", Fix: "free space"},
		{ID: "host.intent", Category: doctor.CatHost, Status: doctor.Pass, Detail: "recorded"},
	})
	r.hostDoctor = func(context.Context, bool) (doctor.Report, error) { return failed, nil }
	summary := &serverInitSummary{}
	err := r.runDoctorGate(context.Background(), summary)
	fault, ok := err.(*contract.Fault)
	if !ok || fault.Code != contract.DoctorFailed {
		t.Fatalf("doctor gate = %v, want doctor_failed", err)
	}
	if !strings.Contains(fault.Hint, "eregion server init --resume") ||
		!strings.Contains(fault.Message, "host.disk") {
		t.Fatalf("doctor_failed fault = %+v", fault)
	}
	checks, _ := fault.Details["checks"].([]doctor.Result)
	if len(checks) != 1 || checks[0].ID != "host.disk" {
		t.Fatalf("fault checks = %+v", fault.Details["checks"])
	}
	if r.pending.Done(phaseDoctor) {
		t.Fatal("a failing gate marked the doctor phase complete")
	}
	if summary.Doctor == nil || summary.Doctor.Summary[doctor.Fail] != 1 {
		t.Fatalf("failed report missing from the summary: %+v", summary.Doctor)
	}

	healthy := doctor.BuildReport([]doctor.Result{{ID: "host.disk", Category: doctor.CatHost, Status: doctor.Pass, Detail: "70% free"}})
	r.hostDoctor = func(context.Context, bool) (doctor.Report, error) { return healthy, nil }
	if err := r.runDoctorGate(context.Background(), summary); err != nil {
		t.Fatalf("resumed gate: %v", err)
	}
	if !r.pending.Done(phaseDoctor) {
		t.Fatal("a passing gate did not mark the doctor phase complete")
	}
	if got := doctor.SummaryLine(summary.Doctor.Summary); got != "1 ok" {
		t.Fatalf("doctor summary line = %q", got)
	}

	// A resume with the phase already recorded restores the durable report and
	// never runs a second bootstrap or doctor check.
	resumed := doctorTestInit(t)
	resumed.pending = r.pending
	resumed.hostDoctor = func(context.Context, bool) (doctor.Report, error) {
		t.Fatal("a passed doctor gate ran again on resume")
		return doctor.Report{}, nil
	}
	resumedSummary := &serverInitSummary{}
	if err := resumed.runDoctorGate(context.Background(), resumedSummary); err != nil {
		t.Fatal(err)
	}
	if resumedSummary.Doctor == nil || doctor.SummaryLine(resumedSummary.Doctor.Summary) != "1 ok" {
		t.Fatalf("restored doctor summary = %+v", resumedSummary.Doctor)
	}
}

func TestDoctorGateSkipWarnsAndMarksThePhase(t *testing.T) {
	r := doctorTestInit(t)
	r.cmd.Flags().Bool("skip-doctor", true, "")
	if err := r.cmd.ParseFlags([]string{"--skip-doctor"}); err != nil {
		t.Fatal(err)
	}
	r.hostDoctor = func(context.Context, bool) (doctor.Report, error) {
		t.Fatal("--skip-doctor reached the node")
		return doctor.Report{}, nil
	}
	summary := &serverInitSummary{}
	if err := r.runDoctorGate(context.Background(), summary); err != nil {
		t.Fatal(err)
	}
	if !r.pending.Done(phaseDoctor) {
		t.Fatal("--skip-doctor did not record the phase")
	}
	if len(r.warnings) != 1 || !strings.Contains(r.warnings[0], "doctor gate was skipped") {
		t.Fatalf("skip warning = %v", r.warnings)
	}
	if summary.Doctor != nil || summary.DoctorSkipped || !r.pending.DoctorSkipped {
		t.Fatalf("skipped gate result = doctor %+v skipped %t pending %+v", summary.Doctor, summary.DoctorSkipped, r.pending)
	}

	resumed := doctorTestInit(t)
	resumed.pending = r.pending
	resumed.hostDoctor = func(context.Context, bool) (doctor.Report, error) {
		t.Fatal("a skipped doctor gate ran again on resume")
		return doctor.Report{}, nil
	}
	resumedSummary := &serverInitSummary{}
	if err := resumed.runDoctorGate(context.Background(), resumedSummary); err != nil {
		t.Fatal(err)
	}
	if resumedSummary.Doctor != nil || !resumedSummary.DoctorSkipped {
		t.Fatalf("restored skipped gate = %+v", resumedSummary)
	}
	if len(resumed.warnings) != 1 || !strings.Contains(resumed.warnings[0], "earlier run") {
		t.Fatalf("restored skip warning = %v", resumed.warnings)
	}
}

func TestResumeAfterDoctorFailureRunsOnlyTheGateAndHandoff(t *testing.T) {
	r := doctorTestInit(t)
	for _, phase := range []string{phaseContact, phaseSecrets, phaseHost, phaseAccessSetup, phaseAccess, phaseContext} {
		r.pending.Mark(phase)
	}
	tasks := strings.Join(r.resumeTasks(), "\n")
	if !strings.Contains(tasks, "check node readiness") || !strings.Contains(tasks, "run the doctor gate") {
		t.Fatalf("resume tasks before the checks = %q", tasks)
	}
	r.pending.Mark(phaseChecks)
	tasks = strings.Join(r.resumeTasks(), "\n")
	if strings.Contains(tasks, "check node readiness") || !strings.Contains(tasks, "run the doctor gate") ||
		!strings.Contains(tasks, "show the secrets handoff") {
		t.Fatalf("resume tasks after the checks = %q", tasks)
	}
	r.pending.Mark(phaseDoctor)
	tasks = strings.Join(r.resumeTasks(), "\n")
	if strings.Contains(tasks, "run the doctor gate") || strings.Contains(tasks, "check node readiness") ||
		!strings.Contains(tasks, "show the secrets handoff") {
		t.Fatalf("resume tasks after the gate passed = %q", tasks)
	}
}

func TestDoctorGateNodeErrorIsNotADoctorFailure(t *testing.T) {
	r := doctorTestInit(t)
	unavailable := contract.NewFault(contract.Unavailable, "node is unreachable", "Reconnect and retry the init.")
	r.hostDoctor = func(context.Context, bool) (doctor.Report, error) { return doctor.Report{}, unavailable }
	summary := &serverInitSummary{}
	err := r.runDoctorGate(context.Background(), summary)
	if !errors.Is(err, unavailable) {
		t.Fatalf("node error = %v, want the transport fault", err)
	}
	if r.pending.Done(phaseDoctor) {
		t.Fatal("an unreachable node marked the doctor phase complete")
	}
}

// initContextTransport answers the node protocol in-process and records the
// methods that reached this context. It proves the init ready and doctor calls
// use the context being initialized.
type initContextTransport struct {
	name    string
	methods []string
}

func (*initContextTransport) Dial(context.Context) (*ssh.Client, error) {
	return nil, errors.New("the fake transport does not dial")
}

func (*initContextTransport) Upload(context.Context, string, []host.RemoteFile) error {
	return nil
}

func (*initContextTransport) Remove(context.Context, string) error { return nil }

func (t *initContextTransport) Run(_ context.Context, _ string, stdin io.Reader, stdout, _ io.Writer) error {
	request, _, err := rpc.DecodeRequest(stdin)
	if err != nil {
		return err
	}
	t.methods = append(t.methods, request.Method)
	var data any
	switch request.Method {
	case methodVersion:
		data = rpcVersionResult{Version: buildinfo.Version, Commit: buildinfo.Commit, OS: "linux", Arch: runtime.GOARCH}
	case methodReady:
		data = map[string]bool{"ready": true}
	case methodHostDoctor:
		data = doctor.BuildReport([]doctor.Result{{ID: "host.disk", Category: doctor.CatHost, Status: doctor.Pass, Detail: "ok"}})
	default:
		return fmt.Errorf("unexpected method %q on %s", request.Method, t.name)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	line, err := rpc.MarshalResult(rpc.Result{OK: true, Data: raw})
	if err != nil {
		return err
	}
	_, err = stdout.Write(line)
	return err
}

func (t *initContextTransport) called(method string) bool {
	for _, got := range t.methods {
		if got == method {
			return true
		}
	}
	return false
}

func TestInitReadyAndDoctorUseTheContextBeingInitialized(t *testing.T) {
	t.Setenv("EREGION_CONTEXT_FILE", filepath.Join(t.TempDir(), "eregion", "contexts.json"))
	t.Setenv("EREGION_CONTEXT", "prod")
	store, err := host.LoadContextStore("")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"staging", "prod"} {
		if _, err := store.SaveContext(host.Context{Name: name, SSH: &host.ContextSSH{
			Address: "10.78.0.1:22", User: "admin", KnownHosts: "/dev/null",
		}}); err != nil {
			t.Fatal(err)
		}
	}
	staging, prod := &initContextTransport{name: "staging"}, &initContextTransport{name: "prod"}
	a := &app{secrets: &secrets.Registry{}, nodeClients: map[string]*nodeClient{
		"staging": {remote: staging, operator: "admin", contextName: "staging", stderr: io.Discard},
		"prod":    {remote: prod, operator: "admin", contextName: "prod", stderr: io.Discard},
	}}
	cmd := &cobra.Command{Use: "init"}
	cmd.SetErr(&bytes.Buffer{})
	r := &serverInit{
		a: a, cmd: cmd, answers: serverInitAnswers{Context: "staging"},
		pending: &host.PendingInit{Server: "staging", Phases: map[string]string{}},
	}
	if err := r.readyNode(context.Background()); err != nil {
		t.Fatalf("init ready: %v", err)
	}
	if !staging.called(methodReady) || prod.called(methodReady) {
		t.Fatalf("ready methods staging=%v prod=%v, want staging only", staging.methods, prod.methods)
	}

	// Resume after the checks: the doctor gate still targets staging even
	// though EREGION_CONTEXT and the current context name prod.
	r.pending.Mark(phaseChecks)
	summary := &serverInitSummary{}
	if err := r.runDoctorGate(context.Background(), summary); err != nil {
		t.Fatalf("resumed doctor gate: %v", err)
	}
	if !staging.called(methodHostDoctor) || prod.called(methodHostDoctor) {
		t.Fatalf("doctor methods staging=%v prod=%v, want staging only", staging.methods, prod.methods)
	}
	if summary.Doctor == nil {
		t.Fatal("doctor gate returned no report")
	}
}
