//go:build linux

package pvccopy

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestAttributeEnumerationFailsClosed(t *testing.T) {
	if err := attributeResult(0, 0); err != nil {
		t.Fatal(err)
	}
	if err := attributeResult(0, syscall.ENOTSUP); err != nil {
		t.Fatal(err)
	}
	for _, errno := range []syscall.Errno{syscall.EPERM, syscall.EACCES, syscall.EIO, syscall.ENOSYS, syscall.ERANGE} {
		if err := attributeResult(0, errno); !errors.Is(err, ErrUnsafe) {
			t.Fatalf("enumeration error permitted: %v", errno)
		}
	}
	if err := attributeResult(1, 0); !errors.Is(err, ErrUnsafe) {
		t.Fatal("visible xattr accepted")
	}
}

func TestLinuxSourceAttributesRefused(t *testing.T) {
	for _, name := range []string{".", "directory", "directory/file"} {
		t.Run(name, func(t *testing.T) {
			source, target := t.TempDir(), t.TempDir()
			writeTestFile(t, source, "directory/file", []byte("keep source bytes"))
			file := filepath.Join(source, name)
			if err := syscall.Setxattr(file, "user.pvccopy-test", []byte("must not disappear"), 0); err != nil {
				t.Fatal(err)
			}
			if _, err := Export(context.Background(), source, testPlan(), io.Discard); !errors.Is(err, ErrUnsafe) {
				t.Fatalf("source attribute silently lost: %v", err)
			}
			if _, err := CopyLocal(context.Background(), source, target, testPlan()); !errors.Is(err, ErrUnsafe) {
				t.Fatalf("source attribute copied: %v", err)
			}
			if _, err := os.Stat(filepath.Join(target, StateDirectory, "complete.json")); !os.IsNotExist(err) {
				t.Fatal("unsupported metadata marked complete")
			}
			value := make([]byte, 64)
			n, err := syscall.Getxattr(file, "user.pvccopy-test", value)
			if err != nil || string(value[:n]) != "must not disappear" {
				t.Fatalf("source attribute mutated: %v", err)
			}
		})
	}
}

// Native Linux POSIX ACL xattr encoding, no setfacl/external tool dependency.
func accessACL() []byte {
	var b []byte
	header := make([]byte, 4)
	binary.LittleEndian.PutUint32(header, 2)
	b = append(b, header...)
	for _, e := range []struct {
		tag, perm uint16
		id        uint32
	}{{1, 7, 0xffffffff}, {2, 4, 1000}, {4, 5, 0xffffffff}, {16, 5, 0xffffffff}, {32, 0, 0xffffffff}} {
		entry := make([]byte, 8)
		binary.LittleEndian.PutUint16(entry, e.tag)
		binary.LittleEndian.PutUint16(entry[2:], e.perm)
		binary.LittleEndian.PutUint32(entry[4:], e.id)
		b = append(b, entry...)
	}
	return b
}

func TestLinuxPOSIXACLRefused(t *testing.T) {
	for _, attribute := range []string{"system.posix_acl_access", "system.posix_acl_default"} {
		t.Run(attribute, func(t *testing.T) {
			source, target := t.TempDir(), t.TempDir()
			if err := syscall.Setxattr(source, attribute, accessACL(), 0); err != nil {
				t.Fatal(err)
			}
			if _, err := CopyLocal(context.Background(), source, target, testPlan()); !errors.Is(err, ErrUnsafe) {
				t.Fatalf("ACL accepted: %v", err)
			}
			if _, err := os.Stat(filepath.Join(target, StateDirectory, "complete.json")); !os.IsNotExist(err) {
				t.Fatal("ACL loss marked complete")
			}
		})
	}
}

func TestLinuxTargetAttributesBlockClaimAndCompletion(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	p := testPlan()
	writeTestFile(t, source, "file", []byte("payload"))
	if err := syscall.Setxattr(target, "user.target", []byte("unsupported"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := CopyLocal(context.Background(), source, target, p); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("target xattr accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, StateDirectory)); !os.IsNotExist(err) {
		t.Fatal("unsupported target claimed")
	}
	if err := syscall.Removexattr(target, "user.target"); err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, err := Export(context.Background(), source, p, writer)
		writer.CloseWithError(err)
		done <- err
	}()
	r, err := Receive(context.Background(), target, p, reader)
	reader.CloseWithError(err)
	exportErr := <-done
	if err != nil || exportErr != nil {
		t.Fatalf("receive: %v %v", err, exportErr)
	}
	if err := syscall.Setxattr(filepath.Join(target, "file"), "user.injected", []byte("new"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := Complete(context.Background(), target, p, r); !errors.Is(err, ErrUnsafe) {
		t.Fatalf("completion ignored injected xattr: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, StateDirectory, "complete.json")); !os.IsNotExist(err) {
		t.Fatal("injected xattr marked complete")
	}
}
