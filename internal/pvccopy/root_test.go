package pvccopy

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// Also runs under a non-root macOS user: group changes are limited to an
// existing supplementary group, never privilege escalation or source chown by
// the copier. Linux root acceptance separately exercises UID 1000 app access.
func TestVolumeRootReceiptAndDifferentGroup(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	p := testPlan()
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	gid := os.Getgid()
	for _, group := range groups {
		if group != gid {
			gid = group
			break
		}
	}
	if gid == os.Getgid() {
		t.Skip("different-group case needs an existing supplementary group")
	}
	if err := os.Chown(source, os.Getuid(), gid); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(source, 0770); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target, 0755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, source, "child", []byte("root access semantics"))
	before, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	var stream bytes.Buffer
	exported, err := Export(context.Background(), source, p, &stream)
	if err != nil {
		t.Fatal(err)
	}
	first, err := tar.NewReader(bytes.NewReader(stream.Bytes())).Next()
	if err != nil {
		t.Fatal(err)
	}
	want := RootReceipt{UID: os.Getuid(), GID: gid, Mode: 0770}
	if first.Name != "." || first.Typeflag != tar.TypeDir || first.Size != 0 || first.Mode != 0770 || first.Uid != want.UID || first.Gid != want.GID || exported.Root != want {
		t.Fatalf("missing bounded root metadata: %+v %+v", first, exported)
	}
	marker, err := CopyLocal(context.Background(), source, target, p)
	if err != nil {
		t.Fatal(err)
	}
	if marker.Receipt != exported || marker.Receipt.Root != want {
		t.Fatalf("root receipt not persisted: %+v", marker.Receipt)
	}
	actual, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	uid, group, ok := ownership(actual)
	if !ok || uid != want.UID || group != want.GID || actual.Mode().Perm() != 0770 {
		t.Fatalf("target root not restored: %v", actual)
	}
	after, err := os.Stat(source)
	if err != nil {
		t.Fatal(err)
	}
	bu, bg, _ := ownership(before)
	au, ag, _ := ownership(after)
	if bu != au || bg != ag || before.Mode() != after.Mode() {
		t.Fatal("copier changed source root")
	}
	// The current non-root application owner can create a new root-level file.
	f, err := os.OpenFile(filepath.Join(target, "application-write"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
}

func TestRootModeIsHashedAndMustRemainExact(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	p := testPlan()
	if err := os.Chmod(source, 0770); err != nil {
		t.Fatal(err)
	}
	m, err := CopyLocal(context.Background(), source, target, p)
	if err != nil {
		t.Fatal(err)
	}
	if m.Receipt.Entries != 1 || m.Receipt.Bytes != 0 || m.Receipt.Root.Mode != 0770 {
		t.Fatalf("empty volume omitted root: %+v", m.Receipt)
	}
	if err := os.Chmod(target, 0755); err != nil {
		t.Fatal(err)
	}
	r, err := export(context.Background(), target, p, io.Discard, true)
	if err != nil {
		t.Fatal(err)
	}
	if r.SHA256 == m.Receipt.SHA256 || r.Root == m.Receipt.Root {
		t.Fatal("root metadata not covered by hash/receipt")
	}
	if _, err := Inspect(context.Background(), target, p); !errors.Is(err, ErrPartial) {
		t.Fatalf("root tampering accepted: %v", err)
	}
}

func TestArchiveRequiresExactlyOneFirstRootDirectory(t *testing.T) {
	p := testPlan()
	for _, tc := range []struct {
		name    string
		headers []*tar.Header
	}{
		{"missing", []*tar.Header{{Name: "child", Typeflag: tar.TypeReg}}},
		{"file-root", []*tar.Header{{Name: ".", Typeflag: tar.TypeReg}}},
		{"root-data", []*tar.Header{{Name: ".", Typeflag: tar.TypeReg, Size: 1}}},
		{"symlink-root", []*tar.Header{{Name: ".", Typeflag: tar.TypeSymlink, Linkname: "outside"}}},
		{"parent", []*tar.Header{{Name: "..", Typeflag: tar.TypeDir}}},
		{"absolute", []*tar.Header{{Name: "/", Typeflag: tar.TypeDir}}},
		{"special-mode", []*tar.Header{{Name: ".", Typeflag: tar.TypeDir, Mode: 01777}}},
		{"duplicate", []*tar.Header{{Name: ".", Typeflag: tar.TypeDir}, {Name: ".", Typeflag: tar.TypeDir}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			tw := tar.NewWriter(&b)
			for _, h := range tc.headers {
				h.Format = tar.FormatUSTAR
				if err := tw.WriteHeader(h); err != nil {
					t.Fatal(err)
				}
				if h.Size != 0 {
					tw.Write([]byte{0})
				}
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := Relay(context.Background(), p, bytes.NewReader(b.Bytes()), io.Discard); err == nil {
				t.Fatal("unsafe root accepted")
			}
		})
	}
	// Root is counted against the entry bound, not a free unbounded manifest.
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, h := range []*tar.Header{{Name: ".", Typeflag: tar.TypeDir, Mode: 0700}, {Name: "child", Typeflag: tar.TypeReg, Mode: 0600}} {
		h.Format = tar.FormatUSTAR
		tw.WriteHeader(h)
	}
	tw.Close()
	p.MaxEntries = 1
	if _, err := Relay(context.Background(), p, bytes.NewReader(b.Bytes()), io.Discard); err == nil {
		t.Fatal("root excluded from entry bound")
	}
}
