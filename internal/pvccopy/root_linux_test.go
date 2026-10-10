//go:build linux

package pvccopy

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

func TestLinuxApplicationRootWriteProbe(t *testing.T) {
	if os.Getenv("PVCCOPY_TEST_APP_PROBE") != "1" {
		return
	}
	path := os.Getenv("PVCCOPY_TEST_APP_PATH")
	f, err := os.OpenFile(filepath.Join(path, "application-created"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if os.Getenv("PVCCOPY_TEST_APP_EXPECT_DENIED") == "1" {
		if !errors.Is(err, os.ErrPermission) {
			t.Fatalf("expected denied app write, got %v", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("UID1000 cannot write restored volume root: %v", err)
	}
	if _, err := f.WriteString("real application write"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(path, "existing-data"))
	if err != nil || string(data) != "physical copy bytes" {
		t.Fatalf("app cannot read copied data: %v", err)
	}
}

func TestLinuxVolumeRootAppUID1000(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("arbitrary UID restoration/app probe requires isolated root test process")
	}
	base := t.TempDir()
	for _, dir := range []string{filepath.Dir(base), base} {
		if err := os.Chmod(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	source, target := filepath.Join(base, "source"), filepath.Join(base, "target")
	for _, dir := range []string{source, target} {
		if err := os.Mkdir(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, source, "existing-data", []byte("physical copy bytes"))
	if err := os.Chown(filepath.Join(source, "existing-data"), 1000, 2000); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(source, 1000, 2000); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(source, 0770); err != nil {
		t.Fatal(err)
	}
	probe := func(denied bool) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=^TestLinuxApplicationRootWriteProbe$", "-test.v")
		cmd.Env = append(os.Environ(), "PVCCOPY_TEST_APP_PROBE=1", "PVCCOPY_TEST_APP_PATH="+target)
		if denied {
			cmd.Env = append(cmd.Env, "PVCCOPY_TEST_APP_EXPECT_DENIED=1")
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: 1000, Gid: 2000, Groups: []uint32{2000}}}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("app probe: %v\n%s", err, out)
		}
	}
	// Fresh target is root:root 0755: prove failure with an actual app process.
	probe(true)
	m, err := CopyLocal(context.Background(), source, target, testPlan())
	if err != nil {
		t.Fatal(err)
	}
	want := RootReceipt{UID: 1000, GID: 2000, Mode: 0770}
	if m.Receipt.Root != want {
		t.Fatalf("wrong root receipt: %+v", m.Receipt)
	}
	s, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	uid, gid, ok := ownership(s)
	if !ok || uid != 1000 || gid != 2000 || s.Mode().Perm() != 0770 {
		t.Fatalf("wrong physical root metadata: %v", s)
	}
	probe(false)
	b, err := os.ReadFile(filepath.Join(target, "application-created"))
	if err != nil || string(b) != "real application write" {
		t.Fatalf("app payload: %v", err)
	}
}
