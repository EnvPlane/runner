package pvccopy

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testPlan() Plan {
	p := Plan{Tenant: "tenant", Project: "project", Environment: "target-env", Source: PVCRef{"source-ns", "source", "source-uid"}, Target: PVCRef{"target-ns", "target", "target-uid"}, MaxBytes: 1 << 20, MaxEntries: 100, Timeout: time.Minute, Image: "registry.example/runner@sha256:" + strings.Repeat("a", 64), AllowedSourceNamespaces: []string{"source-ns"}, SourceQuiescedOffline: true}
	p.AllowedTarget = TargetBinding{p.Tenant, p.Project, p.Environment, p.Target}
	return p
}
func writeTestFile(t *testing.T, root, name string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, name), b, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestLocalCopyBytesAndDurableRetry(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	p := testPlan()
	data := map[string][]byte{"db/ibdata1": bytes.Repeat([]byte{0, 1, 2, 255, 42}, 20000), "db/empty": {}, "other.txt": []byte("offline native bytes\n")}
	for name, b := range data {
		writeTestFile(t, source, name, b)
	}
	if err := os.Mkdir(filepath.Join(source, "empty-dir"), 0700); err != nil {
		t.Fatal(err)
	}
	m, err := CopyLocal(context.Background(), source, target, p)
	if err != nil {
		t.Fatal(err)
	}
	var count int64
	for name, b := range data {
		got, err := os.ReadFile(filepath.Join(target, name))
		if err != nil || !bytes.Equal(got, b) {
			t.Fatalf("copy %s: %v", name, err)
		}
		count += int64(len(b))
	}
	if m.Receipt.Bytes != count || m.Receipt.Entries != 6 || m.SourceUID != p.Source.UID || m.TargetUID != p.Target.UID {
		t.Fatalf("bad marker: %+v", m)
	}
	// Retry proves persisted target bytes without needing the original source.
	retry, err := CopyLocal(context.Background(), "missing-source", target, p)
	if err != nil || retry != m {
		t.Fatalf("retry: %v %+v", err, retry)
	}
	changed := p
	changed.Target.UID = "replacement-uid"
	changed.AllowedTarget.PVC = changed.Target
	if _, err := Inspect(context.Background(), target, changed); !errors.Is(err, ErrPartial) {
		t.Fatalf("replacement accepted: %v", err)
	}
	writeTestFile(t, target, "db/ibdata1", []byte("tampered"))
	if _, err := Inspect(context.Background(), target, p); !errors.Is(err, ErrPartial) {
		t.Fatalf("tampering accepted: %v", err)
	}
}

func TestLocalSafety(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "nonempty", "reserved", "oversize", "entries", "offline", "sameuid", "unbound", "image", "namespace", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			source, target := t.TempDir(), t.TempDir()
			p := testPlan()
			writeTestFile(t, source, "file", []byte("content"))
			ctx := context.Background()
			switch kind {
			case "symlink":
				if err := os.Symlink("file", filepath.Join(source, "link")); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(filepath.Join(source, "file"), filepath.Join(source, "link")); err != nil {
					t.Fatal(err)
				}
			case "nonempty":
				writeTestFile(t, target, "file", []byte("keep"))
			case "reserved":
				writeTestFile(t, source, StateDirectory+"/claim.json", []byte("fake"))
			case "oversize":
				p.MaxBytes = 2
			case "entries":
				p.MaxEntries = 1
				writeTestFile(t, source, "another", []byte("x"))
			case "offline":
				p.SourceQuiescedOffline = false
			case "sameuid":
				p.Target.UID = p.Source.UID
				p.AllowedTarget.PVC = p.Target
			case "unbound":
				p.AllowedTarget.Environment = "other"
			case "image":
				p.Image = "runner:latest"
			case "namespace":
				p.AllowedSourceNamespaces = []string{"other"}
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if _, err := CopyLocal(ctx, source, target, p); err == nil {
				t.Fatal("unsafe copy succeeded")
			}
			if b, err := os.ReadFile(filepath.Join(source, "file")); err != nil || string(b) != "content" {
				t.Fatal("source mutated")
			}
			if kind == "nonempty" {
				b, _ := os.ReadFile(filepath.Join(target, "file"))
				if string(b) != "keep" {
					t.Fatal("overwritten")
				}
			}
			if _, err := os.Stat(filepath.Join(target, StateDirectory, "complete.json")); !os.IsNotExist(err) {
				t.Fatalf("completion exists: %v", err)
			}
		})
	}
}

func archive(t *testing.T, headers []*tar.Header, payload []byte) []byte {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	uid, gid := os.Getuid(), os.Getgid()
	if err := tw.WriteHeader(&tar.Header{Name: ".", Typeflag: tar.TypeDir, Mode: 0700, Uid: uid, Gid: gid, Format: tar.FormatUSTAR}); err != nil {
		t.Fatal(err)
	}
	for _, h := range headers {
		h.Uid = uid
		h.Gid = gid
		if h.Mode == 0 {
			h.Mode = 0600
		}
		if h.Format == tar.FormatUnknown {
			h.Format = tar.FormatUSTAR
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			if _, err := tw.Write(payload[:h.Size]); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}
func TestArchiveSafety(t *testing.T) {
	for _, tc := range []struct {
		name string
		h    tar.Header
	}{
		{"traversal", tar.Header{Name: "../escape", Typeflag: tar.TypeReg}},
		{"absolute", tar.Header{Name: "/escape", Typeflag: tar.TypeReg}},
		{"backslash", tar.Header{Name: `a\b`, Typeflag: tar.TypeReg}},
		{"symlink", tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: "outside"}},
		{"hardlink", tar.Header{Name: "link", Typeflag: tar.TypeLink, Linkname: "file"}},
		{"device", tar.Header{Name: "device", Typeflag: tar.TypeChar}},
		{"fifo", tar.Header{Name: "pipe", Typeflag: tar.TypeFifo}},
		{"setuid", tar.Header{Name: "executable", Typeflag: tar.TypeReg, Mode: 04755}},
		{"setgid", tar.Header{Name: "file", Typeflag: tar.TypeReg, Mode: 02640}},
		{"sticky", tar.Header{Name: "directory", Typeflag: tar.TypeDir, Mode: 01777}},
		{"reserved", tar.Header{Name: StateDirectory + "/complete.json", Typeflag: tar.TypeReg}},
		{"pax", tar.Header{Name: "file", Typeflag: tar.TypeReg, Format: tar.FormatPAX, PAXRecords: map[string]string{"comment": "untrusted"}}},
		{"missing-parent", tar.Header{Name: "dir/file", Typeflag: tar.TypeReg}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := archive(t, []*tar.Header{&tc.h}, nil)
			if _, err := Relay(context.Background(), testPlan(), bytes.NewReader(b), io.Discard); err == nil {
				t.Fatal("accepted unsafe archive")
			}
		})
	}
	t.Run("duplicate", func(t *testing.T) {
		b := archive(t, []*tar.Header{{Name: "a", Typeflag: tar.TypeReg}, {Name: "a", Typeflag: tar.TypeReg}}, nil)
		if _, err := Relay(context.Background(), testPlan(), bytes.NewReader(b), io.Discard); err == nil {
			t.Fatal("duplicate accepted")
		}
	})
	t.Run("truncated", func(t *testing.T) {
		b := archive(t, []*tar.Header{{Name: "a", Typeflag: tar.TypeReg, Size: 3}}, []byte("abc"))
		for _, n := range []int{0, 512, 515, 1024, len(b) - 512} {
			if _, err := Relay(context.Background(), testPlan(), bytes.NewReader(b[:n]), io.Discard); err == nil {
				t.Fatalf("truncation %d accepted", n)
			}
		}
	})
	t.Run("trailing", func(t *testing.T) {
		b := archive(t, nil, nil)
		b = append(b, 1)
		if _, err := Relay(context.Background(), testPlan(), bytes.NewReader(b), io.Discard); err == nil {
			t.Fatal("trailing data accepted")
		}
	})
}

func TestPartialAndVerificationFailClosed(t *testing.T) {
	target := t.TempDir()
	p := testPlan()
	ctx := context.Background()
	b := archive(t, []*tar.Header{{Name: "a", Typeflag: tar.TypeReg, Size: 3}}, []byte("abc"))
	if _, err := Receive(ctx, target, p, bytes.NewReader(b[:513])); err == nil {
		t.Fatal("truncation accepted")
	}
	if _, err := Receive(ctx, target, p, bytes.NewReader(b)); !errors.Is(err, ErrPartial) {
		t.Fatalf("partial retried: %v", err)
	}
	if _, err := Complete(ctx, target, p, Receipt{}); err == nil {
		t.Fatal("partial marked complete")
	}
	clean := t.TempDir()
	r, err := Receive(ctx, clean, p, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	wrong := r
	wrong.SHA256 = strings.Repeat("f", 64)
	if _, err := Complete(ctx, clean, p, wrong); err == nil {
		t.Fatal("wrong hash marked complete")
	}
	if _, err := Inspect(ctx, clean, p); !errors.Is(err, ErrPartial) {
		t.Fatalf("uncommitted target accepted: %v", err)
	}
	if _, err := Complete(ctx, clean, p, r); err != nil {
		t.Fatal(err)
	}
	if _, err := Complete(ctx, clean, p, r); err == nil {
		t.Fatal("marker overwritten")
	}
}

func TestPlanDigestImmutableAndOrderIndependent(t *testing.T) {
	p := testPlan()
	p.AllowedSourceNamespaces = []string{"z", "source-ns"}
	a, err := p.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if p.AllowedSourceNamespaces[0] != "z" {
		t.Fatal("Digest mutated plan")
	}
	p.AllowedSourceNamespaces = []string{"source-ns", "z"}
	b, _ := p.Digest()
	if a != b {
		t.Fatal("unstable policy order")
	}
	p.Source.UID = "new"
	c, _ := p.Digest()
	if a == c {
		t.Fatal("UID omitted from digest")
	}
}

func TestMetadataPreservationAndHash(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	p := testPlan()
	writeTestFile(t, source, "directory/file", []byte("ownership and mode matter"))
	for name, mode := range map[string]os.FileMode{".": 0755, "directory": 0750, "directory/file": 0640} {
		if err := os.Chmod(filepath.Join(source, name), mode); err != nil {
			t.Fatal(err)
		}
	}
	m, err := CopyLocal(context.Background(), source, target, p)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".", "directory", "directory/file"} {
		s, _ := os.Stat(filepath.Join(source, name))
		d, err := os.Stat(filepath.Join(target, name))
		if err != nil {
			t.Fatal(err)
		}
		suid, sgid, sok := ownership(s)
		duid, dgid, dok := ownership(d)
		if !sok || !dok || suid != duid || sgid != dgid || s.Mode().Perm() != d.Mode().Perm() {
			t.Fatalf("metadata differs %s: source %v target %v", name, s, d)
		}
	}
	if err := os.Chmod(filepath.Join(target, "directory/file"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(context.Background(), target, p); !errors.Is(err, ErrPartial) {
		t.Fatalf("mode tampering accepted: %v", err)
	}
	changed, err := export(context.Background(), target, p, io.Discard, true)
	if err != nil {
		t.Fatal(err)
	}
	if changed.SHA256 == m.Receipt.SHA256 {
		t.Fatal("mode not hashed")
	}
	ha, hb := sha256.New(), sha256.New()
	header := &tar.Header{Name: "file", Typeflag: tar.TypeReg, Mode: 0640, Uid: 1000, Gid: 1000}
	hashHeader(ha, header)
	header.Uid = 2000
	hashHeader(hb, header)
	if bytes.Equal(ha.Sum(nil), hb.Sum(nil)) {
		t.Fatal("uid not hashed")
	}
}

func TestSpecialPermissionsAndUnsupportedOwnership(t *testing.T) {
	for _, mode := range []os.FileMode{0600 | os.ModeSetuid, 0600 | os.ModeSetgid, 0700 | os.ModeSticky} {
		source, target := t.TempDir(), t.TempDir()
		writeTestFile(t, source, "file", []byte("data"))
		if err := os.Chmod(filepath.Join(source, "file"), mode); err != nil {
			t.Fatal(err)
		}
		actual, err := os.Stat(filepath.Join(source, "file"))
		if err != nil {
			t.Fatal(err)
		}
		if actual.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) == 0 {
			t.Logf("host stripped special permission %v", mode)
			continue
		}
		if _, err := CopyLocal(context.Background(), source, target, testPlan()); err == nil {
			t.Fatalf("special permissions allowed: %v", mode)
		}
	}
	if os.Geteuid() == 0 {
		t.Skip("non-root ownership refusal requires a non-root test process")
	}
	target := t.TempDir()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, h := range []*tar.Header{{Name: ".", Typeflag: tar.TypeDir, Mode: 0700, Uid: os.Getuid(), Gid: os.Getgid(), Format: tar.FormatUSTAR}, {Name: "other-owner", Typeflag: tar.TypeReg, Mode: 0600, Uid: os.Getuid() + 1, Gid: os.Getgid(), Format: tar.FormatUSTAR}} {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Receive(context.Background(), target, testPlan(), bytes.NewReader(b.Bytes())); err == nil {
		t.Fatal("unsupported ownership accepted")
	}
	if _, err := os.Stat(filepath.Join(target, StateDirectory, "complete.json")); !os.IsNotExist(err) {
		t.Fatal("ownership failure marked complete")
	}
}
