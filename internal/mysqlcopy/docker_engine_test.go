package mysqlcopy

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// This opt-in fixture tests real MySQL lock/dump/restore semantics, not Kubernetes
// admission/TLS enforcement. It creates only new disposable networkless servers,
// no host-volume mounts, and keeps credential files/dumps in RAM. Deployment
// acceptance separately requires the native Kube driver/admission live harness.
const engineImage = "mysql@sha256:7dcddc01f13bab2f15cde676d44d01f61fc9f99fe7785e86196dfc07d358ae2b"

func dockerRun(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdin = in
	cmd.Stdout = out
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 2 * time.Second
	return cmd.Run()
}
func engineSQL(ctx context.Context, id, config, sql string, out io.Writer) error {
	return dockerRun(ctx, []string{"exec", "-i", id, "mysql", "--defaults-extra-file=" + config, "--protocol=SOCKET", "--socket=/var/run/mysqld/mysqld.sock", "--skip-reconnect", "--batch", "--skip-column-names", "--unbuffered"}, strings.NewReader(sql), out)
}
func fixtureServer(t *testing.T, ctx context.Context) string {
	t.Helper()
	b := &bytes.Buffer{}
	args := []string{"run", "-d", "--pull=never", "--network=none", "--security-opt=no-new-privileges:true", "--tmpfs", "/var/lib/mysql:rw,nosuid,size=1g", "--tmpfs", "/config:rw,noexec,nosuid,size=1m", "--tmpfs", "/var/run/mysqld:rw,nosuid,size=16m", "--env", "MYSQL_ALLOW_EMPTY_PASSWORD=yes", engineImage}
	if err := dockerRun(ctx, args, nil, b); err != nil {
		t.Fatal("cannot start disposable MySQL fixture (details redacted)")
	}
	id := strings.TrimSpace(b.String())
	if len(id) != 64 {
		t.Fatal("invalid fixture identity")
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if dockerRun(cleanup, []string{"rm", "-f", id}, nil, io.Discard) != nil {
			t.Error("fixture cleanup failed; only fixture id", id)
		}
	})
	// Newly created networkless fixture alone uses empty bootstrap root. No
	// existing database is contacted, and no password appears in process argv.
	for {
		comm := &bytes.Buffer{}
		running := dockerRun(ctx, []string{"exec", id, "cat", "/proc/1/comm"}, nil, comm) == nil && strings.TrimSpace(comm.String()) == "mysqld"
		if running && dockerRun(ctx, []string{"exec", "-i", id, "mysql", "-uroot", "--protocol=SOCKET", "-e", "SELECT 1"}, nil, io.Discard) == nil {
			return id
		}
		if ctx.Err() != nil {
			t.Fatal("fixture startup timed out")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
func fixtureConfig(t *testing.T, ctx context.Context, id, path, user, password string) {
	t.Helper()
	b, err := ClientOptions(user, password)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(b)
	if dockerRun(ctx, []string{"exec", "-i", id, "tee", path}, bytes.NewReader(b), io.Discard) != nil {
		t.Fatal("fixture configuration failed")
	}
	if dockerRun(ctx, []string{"exec", id, "chmod", "0600", path}, nil, io.Discard) != nil {
		t.Fatal("fixture config permissions")
	}
}
func fixtureBootstrap(t *testing.T, ctx context.Context, id, sql string) {
	t.Helper()
	if dockerRun(ctx, []string{"exec", "-i", id, "mysql", "-uroot", "--protocol=SOCKET"}, strings.NewReader(sql), io.Discard) != nil {
		t.Fatal("fixture bootstrap failed (details redacted)")
	}
}

func TestMySQL8EngineConsistentFixture(t *testing.T) {
	if os.Getenv("ENVPLANE_MYSQLCOPY_DOCKER_TEST") != "1" {
		t.Skip("opt-in disposable MySQL8 engine fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	source := fixtureServer(t, ctx)
	target := fixtureServer(t, ctx)
	fixtureBootstrap(t, ctx, source, "CREATE DATABASE app; CREATE USER 'backupreader'@'localhost' IDENTIFIED BY 'fixture-source-read'; GRANT SELECT, SHOW VIEW ON app.* TO 'backupreader'@'localhost'; CREATE USER 'backupadmin'@'localhost' IDENTIFIED BY 'fixture-backup-admin'; GRANT BACKUP_ADMIN ON *.* TO 'backupadmin'@'localhost'; CREATE USER 'writer'@'localhost' IDENTIFIED BY 'fixture-writer'; GRANT INSERT ON app.* TO 'writer'@'localhost'; CREATE TABLE app.records(id BIGINT PRIMARY KEY,payload VARBINARY(128)) ENGINE=InnoDB; INSERT INTO app.records VALUES(0,0x00FF273B0A);\n")
	fixtureBootstrap(t, ctx, target, "CREATE DATABASE app; CREATE USER 'app'@'localhost' IDENTIFIED BY 'fixture-target-app'; GRANT ALL ON app.* TO 'app'@'localhost'; ALTER USER 'root'@'localhost' IDENTIFIED BY 'fixture-independent-target-root';\n")
	fixtureBootstrap(t, ctx, source, "SET SESSION SQL_MODE='NO_AUTO_VALUE_ON_ZERO'; ALTER TABLE app.records MODIFY id BIGINT NOT NULL AUTO_INCREMENT; GRANT SELECT, SHOW VIEW, TRIGGER, EVENT ON app.* TO 'backupadmin'@'localhost'; GRANT SHOW_ROUTINE ON *.* TO 'backupadmin'@'localhost';\n")
	fixtureConfig(t, ctx, source, "/config/app.cnf", "backupreader", "fixture-source-read")
	fixtureConfig(t, ctx, source, "/config/admin.cnf", "backupadmin", "fixture-backup-admin")
	fixtureConfig(t, ctx, source, "/config/writer.cnf", "writer", "fixture-writer")
	fixtureConfig(t, ctx, target, "/config/app.cnf", "app", "fixture-target-app")
	grants := &bytes.Buffer{}
	if engineSQL(ctx, source, "/config/admin.cnf", "SHOW GRANTS FOR CURRENT_USER;\n", grants) != nil || !catalogAuthority("app", grants.String()) {
		t.Fatal("real admin catalog authority not proven")
	}
	// Demonstrate actual catalog filtering: a SELECT-only dump principal's zero
	// trigger count is not absence proof. The approved metadata admin sees it.
	fixtureBootstrap(t, ctx, source, "CREATE TRIGGER app.catalog_probe BEFORE INSERT ON app.records FOR EACH ROW SET NEW.payload=NEW.payload;\n")
	for _, probe := range []struct{ config, want string }{{"/config/app.cnf", "0"}, {"/config/admin.cnf", "1"}} {
		count := &bytes.Buffer{}
		if engineSQL(ctx, source, probe.config, "SELECT COUNT(*) FROM information_schema.triggers WHERE trigger_schema='app';\n", count) != nil || strings.TrimSpace(count.String()) != probe.want {
			t.Fatal("catalog visibility fixture did not demonstrate hidden object")
		}
	}
	fixtureBootstrap(t, ctx, source, "DROP TRIGGER app.catalog_probe;\n")
	uuid := &bytes.Buffer{}
	if engineSQL(ctx, source, "/config/app.cnf", "SELECT @@server_uuid;\n", uuid) != nil {
		t.Fatal("fixture UUID query")
	}
	operation, stopOperation := context.WithCancel(ctx)
	defer stopOperation()
	session, stopSession := backupSessionContext(operation, 2*time.Minute)
	defer stopSession()
	input, writer := io.Pipe()
	output, sink := io.Pipe()
	h := &backupHold{in: writer, out: bufio.NewReaderSize(output, 4096), reader: output, done: make(chan error, 1), cancel: stopSession, uuid: strings.TrimSpace(uuid.String())}
	go func() {
		err := dockerRun(session, []string{"exec", "-i", source, "mysql", "--defaults-extra-file=/config/admin.cnf", "--binary-mode=0", "--local-infile=0", "--skip-reconnect", "--batch", "--skip-column-names", "--unbuffered"}, input, sink)
		_ = input.CloseWithError(err)
		_ = sink.CloseWithError(err)
		h.done <- err
	}() // pipe cleanup; checked via hold
	stop := context.AfterFunc(session, func() { _ = writer.CloseWithError(session.Err()); _ = output.CloseWithError(session.Err()) })
	defer stop()
	if _, err := io.WriteString(writer, "SET SESSION lock_wait_timeout=5; LOCK INSTANCE FOR BACKUP;\n"); err != nil {
		t.Fatal("fixture backup lock")
	}
	if h.Check(ctx) != nil {
		t.Fatal("backup lock challenge failed")
	}
	defer func() {
		// Exercise cancellation before release with the production session lifetime.
		stopOperation()
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if h.Release(cleanup) != nil {
			t.Error("backup lock release")
			return
		}
		// Actual DDL succeeding after acknowledged unlock proves the fixture's
		// server no longer holds the lock; a local process exit alone is not proof.
		if engineSQL(cleanup, source, "/config/admin.cnf", "SELECT 1;\n", io.Discard) != nil {
			t.Error("source unavailable after cancelled-operation release")
		}
		if dockerRun(cleanup, []string{"exec", "-i", source, "mysql", "-uroot", "--protocol=SOCKET"}, strings.NewReader("SET SESSION lock_wait_timeout=1; ALTER TABLE app.records ADD COLUMN released INT;\n"), io.Discard) != nil {
			t.Error("actual DDL remained blocked after release")
		}
	}()
	// DDL must actually be blocked. Snapshot metadata comparisons are not proof.
	ddlCtx, stopDDL := context.WithTimeout(ctx, 3*time.Second)
	err := dockerRun(ddlCtx, []string{"exec", "-i", source, "mysql", "-uroot", "--protocol=SOCKET"}, strings.NewReader("SET SESSION lock_wait_timeout=1; ALTER TABLE app.records ADD COLUMN forbidden INT;\n"), io.Discard)
	stopDDL()
	if err == nil {
		t.Fatal("DDL succeeded under backup lock")
	}
	writerCtx, stopWriter := context.WithCancel(ctx)
	defer stopWriter()
	wRead, wWrite := io.Pipe()
	writerDone := make(chan error, 1)
	go func() {
		err := dockerRun(writerCtx, []string{"exec", "-i", source, "mysql", "--defaults-extra-file=/config/writer.cnf", "--skip-reconnect", "--database=app"}, wRead, io.Discard)
		_ = wRead.CloseWithError(err)
		writerDone <- err
	}()
	insertsDone := make(chan struct{})
	go func() {
		defer close(insertsDone)
		for {
			if _, err := io.WriteString(wWrite, "INSERT INTO records(payload) VALUES(0x00FF273B0A);\n"); err != nil {
				return
			}
			select {
			case <-writerCtx.Done():
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	p := testPlan()
	for {
		count := &bytes.Buffer{}
		if engineSQL(ctx, source, "/config/app.cnf", "SELECT COUNT(*) FROM app.records;\n", count) != nil {
			t.Fatal("writer readiness")
		}
		rows, err := strconv.ParseInt(strings.TrimSpace(count.String()), 10, 64)
		if err == nil && rows >= 20 {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("writer did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	p.MaxRows = 100000
	p.MaxBytes = 16 << 20
	sourceTrack := newTracker(p)
	reader, archiveWriter := io.Pipe()
	dumpDone := make(chan error, 1)
	// Fixture connections use Unix sockets in isolated networkless containers.
	// Production uses the same dump profile with mandatory VERIFY_IDENTITY TLS.
	fixtureArgs := func(id string, dump bool) []string {
		a := dumpArgs(p, true)
		if !dump {
			a = clientArgs(p, true, false)
		}
		for i, v := range a {
			if v == "--socket=/run/mysqld/mysqld.sock" {
				a[i] = "--socket=/var/run/mysqld/mysqld.sock"
			}
		}
		return append([]string{"exec", "-i", id}, a...)
	}
	go func() {
		err := dockerRun(ctx, fixtureArgs(source, true), nil, archiveWriter)
		_ = archiveWriter.CloseWithError(err)
		dumpDone <- err
	}()
	err = dockerRun(ctx, fixtureArgs(target, false), io.MultiReader(strings.NewReader("SET SESSION SQL_MODE='NO_AUTO_VALUE_ON_ZERO'; SET SESSION TIME_ZONE='+00:00'; SET SESSION FOREIGN_KEY_CHECKS=0;\n"), validateReader(reader, sourceTrack)), io.Discard)
	_ = reader.CloseWithError(err)
	dumpErr := <-dumpDone
	if err != nil || dumpErr != nil {
		t.Fatalf("logical dump/restore failed: restore=%v dump=%v parserFailed=%t pendingBytes=%d (SQL/credentials redacted)", err != nil, dumpErr != nil, sourceTrack.failed, sourceTrack.pending.Len())
	}
	proof, err := sourceTrack.Finish()
	if err != nil {
		t.Fatal("actual dump profile not supported", err)
	}
	restored := newTracker(p)
	if dockerRun(ctx, fixtureArgs(target, true), nil, restored) != nil {
		t.Fatal("target re-dump")
	}
	actual, err := restored.Finish()
	if err != nil || !sameProof(proof, actual) {
		t.Fatal("restored schema/rows differ")
	}
	time.Sleep(20 * time.Millisecond)
	stopWriter()
	_ = wWrite.Close()
	<-insertsDone
	<-writerDone
	count := &bytes.Buffer{}
	if engineSQL(ctx, source, "/config/app.cnf", "SELECT COUNT(*) FROM app.records;\n", count) != nil {
		t.Fatal("source DML verification")
	}
	rows, err := strconv.ParseInt(strings.TrimSpace(count.String()), 10, 64)
	if err != nil || rows <= proof.Tables[0].Rows {
		t.Fatal("separate DML writer did not continue through transactional dump")
	}
	if h.Check(ctx) != nil {
		t.Fatal("backup lock lost")
	}
	// Exercise the production signal protocol as the server's unprivileged UID,
	// not Docker's default root exec user and not a nonexistent kill executable.
	if dockerRun(ctx, []string{"exec", "--user", "999:999", target, "sh", "-c", "kill -TERM 1"}, nil, io.Discard) != nil {
		t.Fatal("same-UID graceful shutdown signal failed")
	}
	exit := &bytes.Buffer{}
	if dockerRun(ctx, []string{"wait", target}, nil, exit) != nil || strings.TrimSpace(exit.String()) != "0" {
		t.Fatal("MySQL did not shut down cleanly")
	}
	t.Logf("real MySQL8 restored %d snapshot rows/%d dump bytes; source writer advanced to %d rows", proof.Tables[0].Rows, proof.Bytes, rows)
}
