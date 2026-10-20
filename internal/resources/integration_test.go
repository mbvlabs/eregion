package resources

import (
	"context"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestIntegrationPostgreSQLDrill is opt-in because it starts a disposable
// PostgreSQL container, a named volume, and a published loopback port. It uses
// unique names and never touches an existing database. Run with:
//
//	EREGION_TEST_RESOURCES_DOCKER=1 go test ./internal/resources -run Integration -v
func TestIntegrationPostgreSQLDrill(t *testing.T) {
	if os.Getenv("EREGION_TEST_RESOURCES_DOCKER") != "1" {
		t.Skip("set EREGION_TEST_RESOURCES_DOCKER=1 to run the disposable Docker/PostgreSQL drill")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	host := checkDocker(t, ctx)

	suffix, err := randomHex(6)
	if err != nil {
		t.Fatal(err)
	}
	name := "itest-" + suffix[:12]
	db := "itest_" + suffix[:12]
	spec := Spec{
		Name: name, Engine: EnginePostgreSQL, Version: "17", Host: host,
		Address: "127.0.0.1", Port: 0, Database: db, Username: "app_user",
		Protection: ProtectionApplication,
	}
	stateDir := t.TempDir()
	backupDir := t.TempDir()
	service, err := New(Config{
		Host: host, StateDir: stateDir, BackupDir: backupDir, Runner: ExecRunner{},
		CommandTimeout: 2 * time.Minute, StartTimeout: 2 * time.Minute, PollInterval: 250 * time.Millisecond,
		ControlPlaneDatabase: "eregion_control_plane_never_matches",
	})
	if err != nil {
		t.Fatal(err)
	}
	spec, err = service.normalizeSpec(spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanup := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanup()
		_ = exec.CommandContext(cleanupCtx, "docker", "rm", "--force", containerName(spec)).Run()
		_ = exec.CommandContext(cleanupCtx, "docker", "volume", "rm", spec.Volume).Run()
	})
	if err := os.Remove(filepath.Join(backupDir, ".keep")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	created, err := service.Execute(ctx, Request{Action: ActionCreate, Spec: spec, OperationID: "drill-create"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.State != StateReady || created.Endpoint == nil || created.Endpoint.Port == 0 {
		t.Fatalf("unexpected created resource: %+v", created)
	}
	createReplay, err := service.Execute(ctx, Request{Action: ActionCreate, Spec: spec, OperationID: "drill-create"})
	if err != nil || !createReplay.Replayed || createReplay.Endpoint == nil || createReplay.Endpoint.Port != created.Endpoint.Port {
		t.Fatalf("create retry was not replayed from durable evidence: result=%+v err=%v", createReplay, err)
	}
	password, err := os.ReadFile(created.Credential.PasswordFile)
	if err != nil {
		t.Fatal(err)
	}
	connectionString := integrationDSN(created.Endpoint, string(password))
	connection, err := pgx.Connect(ctx, connectionString)
	if err != nil {
		t.Fatalf("application connection: %v", err)
	}
	defer func() { _ = connection.Close(context.Background()) }()
	if _, err := connection.Exec(ctx, "CREATE TABLE eregion_drill (marker text primary key)"); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, "INSERT INTO eregion_drill(marker) VALUES ('before-backup')"); err != nil {
		t.Fatal(err)
	}

	health, err := service.Execute(ctx, Request{Action: ActionHealth, Spec: spec})
	if err != nil || health.Health == nil || health.Health.Status != HealthHealthy {
		t.Fatalf("health=%+v err=%v", health.Health, err)
	}

	backup, err := service.Execute(ctx, Request{Action: ActionBackup, Spec: spec, OperationID: "drill-backup-1", Backup: &BackupRequest{Reason: "manual"}})
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	backupReplay, err := service.Execute(ctx, Request{Action: ActionBackup, Spec: spec, OperationID: "drill-backup-1", Backup: &BackupRequest{Reason: "manual"}})
	if err != nil || !backupReplay.Replayed || backupReplay.Backup == nil || backupReplay.Backup.Archive != backup.Backup.Archive {
		t.Fatalf("backup retry was not replayed with the same archive: result=%+v err=%v", backupReplay, err)
	}
	if listed, err := service.ListBackups(); err != nil || len(listed) != 1 {
		t.Fatalf("backup retry created a duplicate archive: %d err=%v", len(listed), err)
	}
	verified, err := service.Execute(ctx, Request{Action: ActionVerify, Spec: spec, Verify: &VerifyRequest{Archive: backup.Backup.Archive}})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !verified.Backup.Verified {
		t.Fatal("backup was not verified")
	}
	if _, err := connection.Exec(ctx, "INSERT INTO eregion_drill(marker) VALUES ('after-backup')"); err != nil {
		t.Fatal(err)
	}

	restored, err := service.Execute(ctx, Request{
		Action: ActionRestore, Spec: spec, OperationID: "drill-restore-1",
		Restore: &RestoreRequest{Archive: backup.Backup.Archive, Confirmation: name},
	})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored.SafetyBackup == nil || restored.SafetyBackup.Reason != "safety" {
		t.Fatalf("restore did not retain a safety backup: %+v", restored)
	}
	if !integrationMarkers(ctx, t, connectionString).only("before-backup") {
		t.Fatal("restore did not return the database to the archived state")
	}
	restoreReplay, err := service.Execute(ctx, Request{
		Action: ActionRestore, Spec: spec, OperationID: "drill-restore-1",
		Restore: &RestoreRequest{Archive: backup.Backup.Archive, Confirmation: name},
	})
	if err != nil || !restoreReplay.Replayed || restoreReplay.SafetyBackup == nil || restoreReplay.SafetyBackup.Archive != restored.SafetyBackup.Archive {
		t.Fatalf("restore retry was not finalized from durable evidence: result=%+v err=%v", restoreReplay, err)
	}
	// Cutover terminated the old application sessions. Reconnect as an
	// application would after a restore.
	_ = connection.Close(context.Background())
	connection, err = pgx.Connect(ctx, connectionString)
	if err != nil {
		t.Fatalf("reconnect after restore: %v", err)
	}

	// A failed restore must restore the pre-restore database, not leave the
	// temporary database serving. Inject one health-probe failure after cutover.
	failingBackup, err := service.Execute(ctx, Request{Action: ActionBackup, Spec: spec, OperationID: "drill-backup-2"})
	if err != nil {
		t.Fatalf("second backup: %v", err)
	}
	if _, err := connection.Exec(ctx, "INSERT INTO eregion_drill(marker) VALUES ('after-second-backup')"); err != nil {
		t.Fatal(err)
	}
	_ = connection.Close(context.Background())
	faulty, err := New(Config{
		Host: host, StateDir: stateDir, BackupDir: backupDir,
		Runner:         failReadProbeRunner{base: ExecRunner{}, database: db},
		CommandTimeout: 2 * time.Minute, StartTimeout: 2 * time.Minute, PollInterval: 250 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	failed, restoreErr := faulty.Execute(ctx, Request{
		Action: ActionRestore, Spec: spec, OperationID: "drill-restore-2",
		Restore: &RestoreRequest{Archive: failingBackup.Backup.Archive, Confirmation: name},
	})
	if restoreErr == nil {
		t.Fatal("injected post-cutover probe failure reported a successful restore")
	}
	if failed.Recovery == nil || !failed.Recovery.Succeeded {
		t.Fatalf("failed restore did not recover: %+v recovery=%+v", restoreErr, failed.Recovery)
	}
	if !integrationMarkers(ctx, t, connectionString).only("before-backup", "after-second-backup") {
		t.Fatal("failed restore did not recover the pre-restore database")
	}

	destroyed, err := service.Execute(ctx, Request{
		Action: ActionDestroy, Spec: spec, OperationID: "drill-destroy",
		Destroy: &DestroyRequest{Confirmation: name, RemoveVolume: true},
	})
	if err != nil {
		t.Fatalf("destroy: %v", err)
	}
	if destroyed.State != StateAbsent {
		t.Fatalf("unexpected destroy result: %+v", destroyed)
	}
	destroyReplay, err := service.Execute(ctx, Request{
		Action: ActionDestroy, Spec: spec, OperationID: "drill-destroy",
		Destroy: &DestroyRequest{Confirmation: name, RemoveVolume: true},
	})
	if err != nil || !destroyReplay.Replayed || destroyReplay.State != StateAbsent {
		t.Fatalf("destroy retry was not replayed: result=%+v err=%v", destroyReplay, err)
	}
	containerLeft := dockerObjectExists(ctx, "container", containerName(spec))
	volumeLeft := dockerObjectExists(ctx, "volume", spec.Volume)
	if containerLeft || volumeLeft {
		t.Fatalf("destroy left the container=%v or volume=%v behind", containerLeft, volumeLeft)
	}
}

type markerSet map[string]int

func (m markerSet) only(want ...string) bool {
	if len(m) != len(want) {
		return false
	}
	for _, marker := range want {
		if m[marker] != 1 {
			return false
		}
	}
	return true
}

func integrationMarkers(ctx context.Context, t *testing.T, dsn string) markerSet {
	t.Helper()
	connection, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	defer func() { _ = connection.Close(context.Background()) }()
	rows, err := connection.Query(ctx, "SELECT marker, count(*) FROM eregion_drill GROUP BY marker")
	if err != nil {
		t.Fatalf("read markers: %v", err)
	}
	defer rows.Close()
	out := markerSet{}
	for rows.Next() {
		var marker string
		var count int
		if err := rows.Scan(&marker, &count); err != nil {
			t.Fatal(err)
		}
		out[marker] = count
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func integrationDSN(endpoint *Endpoint, password string) string {
	user := url.UserPassword(endpoint.Username, password)
	return (&url.URL{
		Scheme: "postgres", User: user,
		Host: net.JoinHostPort(endpoint.Address, strconv.Itoa(endpoint.Port)),
		Path: "/" + endpoint.Database, RawQuery: "sslmode=disable",
	}).String()
}

type failReadProbeRunner struct {
	base     Runner
	database string
}

func (r failReadProbeRunner) NodeIdentity(ctx context.Context) (string, error) {
	return r.base.NodeIdentity(ctx)
}

func (r failReadProbeRunner) Run(ctx context.Context, command Command) (CommandResult, error) {
	if command.Name == "docker" && contains(command.Args, "--command=SELECT 1") && contains(command.Args, "--dbname="+r.database) {
		return CommandResult{ExitCode: 1, Stderr: []byte("injected post-cutover read probe failure")}, nil
	}
	return r.base.Run(ctx, command)
}

func checkDocker(t *testing.T, ctx context.Context) string {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Fatal("EREGION_TEST_RESOURCES_DOCKER=1 requires Docker")
	}
	if output, err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}").CombinedOutput(); err != nil {
		t.Fatalf("Docker is not reachable: %v: %s", err, strings.TrimSpace(string(output)))
	}
	if output, err := exec.CommandContext(ctx, "docker", "image", "inspect", "postgres:17").CombinedOutput(); err != nil {
		t.Fatalf("postgres:17 image is required (pull it explicitly): %v: %s", err, strings.TrimSpace(string(output)))
	}
	output, err := exec.CommandContext(ctx, "docker", "info", "--format", "{{.Name}}").Output()
	if err != nil {
		t.Fatalf("read Docker node identity: %v", err)
	}
	identity := strings.TrimSpace(string(output))
	if identity == "" {
		t.Fatal("Docker node identity is empty; a Host string cannot stand in for execution identity")
	}
	return identity
}

func dockerObjectExists(ctx context.Context, kind, name string) bool {
	command := exec.CommandContext(ctx, "docker", kind, "inspect", name)
	output, err := command.CombinedOutput()
	if err == nil {
		return true
	}
	if strings.Contains(strings.ToLower(string(output)), "no such") {
		return false
	}
	return true
}
