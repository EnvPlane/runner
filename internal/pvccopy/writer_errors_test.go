package pvccopy

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
)

type failingPayloadWriter struct {
	writeErr error
	closeErr error
	closed   bool
}

func (w *failingPayloadWriter) Write(p []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return len(p), nil
}
func (w *failingPayloadWriter) Close() error { w.closed = true; return w.closeErr }

func TestConsumePreservesPayloadWriteAndCloseFailures(t *testing.T) {
	writeErr, closeErr := errors.New("payload write failed"), errors.New("payload close failed")
	for _, withWriteError := range []bool{false, true} {
		t.Run(map[bool]string{false: "close-only", true: "write-and-close"}[withWriteError], func(t *testing.T) {
			writer := &failingPayloadWriter{closeErr: closeErr}
			if withWriteError {
				writer.writeErr = writeErr
			}
			data := archive(t, []*tar.Header{{Name: "file", Typeflag: tar.TypeReg, Size: 3}}, []byte("abc"))
			_, err := consume(context.Background(), testPlan(), bytes.NewReader(data), func(h *tar.Header) (io.WriteCloser, error) {
				if h.Typeflag == tar.TypeDir {
					return nil, nil
				}
				return writer, nil
			}, nil)
			if !writer.closed || !errors.Is(err, closeErr) {
				t.Fatalf("close failure was lost: %v", err)
			}
			if withWriteError && !errors.Is(err, writeErr) {
				t.Fatalf("write failure was lost: %v", err)
			}
		})
	}
}

type failAfterWriter struct {
	remaining int
	err       error
}

func (w *failAfterWriter) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		return 0, w.err
	}
	w.remaining -= len(p)
	return len(p), nil
}

func TestArchiveFinalizationErrorsPropagate(t *testing.T) {
	ctx, p := context.Background(), testPlan()
	endErr := errors.New("tar footer write failed")
	// The mandatory root header is one 512-byte block; failure occurs only when
	// tar.Close writes its end blocks, not during source reads or payload writes.
	if _, err := Export(ctx, t.TempDir(), p, &failAfterWriter{remaining: 512, err: endErr}); !errors.Is(err, endErr) {
		t.Fatalf("export footer error lost: %v", err)
	}
	data := archive(t, nil, nil)
	if _, err := Relay(ctx, p, bytes.NewReader(data), &failAfterWriter{remaining: 512, err: endErr}); !errors.Is(err, endErr) {
		t.Fatalf("relay footer error lost: %v", err)
	}
}
