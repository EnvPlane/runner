package mysqlcopy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestOnlyFixedBackupProtocolAllowsQuit(t *testing.T) {
	p := testPlan()
	if !slices.Contains(clientArgs(p, true, false), "--binary-mode=1") || slices.Contains(backupClientArgs(p), "--binary-mode=1") || !slices.Contains(backupClientArgs(p), "--binary-mode=0") || !slices.Contains(backupClientArgs(p), "--local-infile=0") {
		t.Fatal("unsafe client command profile")
	}
}

func TestBackupSessionSurvivesOperationCancelButRemainsBounded(t *testing.T) {
	operation, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	session, stop := backupSessionContext(operation, time.Hour)
	defer stop()
	operationDeadline, _ := operation.Deadline()
	sessionDeadline, _ := session.Deadline()
	cancel()
	if session.Err() != nil || !sessionDeadline.Equal(operationDeadline.Add(5*time.Second)) {
		t.Fatal("unbounded or prematurely cancelled lock session")
	}
}

func TestBackupReleaseRequiresSameSessionAckAndCleanExit(t *testing.T) {
	for _, tc := range []struct {
		name, ack string
		exit      error
		want      bool
	}{
		{"valid", "envplane_unlock\t42\tserver-uuid\n", nil, true},
		{"wrong-connection", "envplane_unlock\t43\tserver-uuid\n", nil, false},
		{"wrong-server", "envplane_unlock\t42\tforeign\n", nil, false},
		{"lost-ack", "", nil, false},
		{"failed-exit", "envplane_unlock\t42\tserver-uuid\n", errors.New("password=secret"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input, writer := io.Pipe()
			output, sink := io.Pipe()
			session, stop := context.WithCancel(context.Background())
			defer stop()
			h := &backupHold{in: writer, out: bufio.NewReader(output), reader: output, done: make(chan error, 1), cancel: stop, uuid: "server-uuid", connectionID: 42}
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				defer func() { _ = input.Close(); _ = sink.Close() }() // fixture pipe cleanup
				scanner := bufio.NewScanner(input)
				if !scanner.Scan() || scanner.Text() != "UNLOCK INSTANCE;" {
					h.done <- ErrUnsafe
					return
				}
				if !scanner.Scan() || scanner.Text() != "SELECT 'envplane_unlock', CONNECTION_ID(), @@server_uuid;" {
					h.done <- ErrUnsafe
					return
				}
				if tc.ack == "" {
					<-session.Done()
					return
				}
				if _, err := io.WriteString(sink, tc.ack); err != nil {
					h.done <- err
					return
				}
				if scanner.Scan() && scanner.Text() == "quit" {
					h.done <- tc.exit
					return
				}
				h.done <- ErrUnsafe
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			err := h.Release(ctx)
			if (err == nil) != tc.want {
				t.Fatal("incorrect release result", tc.name, err)
			}
			if err != nil && strings.Contains(safeFailure(ReleaseDDL, err).Error(), "secret") {
				t.Fatal("release error body leaked")
			}
			select {
			case <-finished:
			case <-time.After(time.Second):
				t.Fatal("release leaked fixture session")
			}
		})
	}
}

func TestBackupReleaseAfterOperationCancellation(t *testing.T) {
	operation, cancel := context.WithCancel(context.Background())
	defer cancel()
	session, stop := backupSessionContext(operation, time.Second)
	defer stop()
	input, writer := io.Pipe()
	output, sink := io.Pipe()
	h := &backupHold{in: writer, out: bufio.NewReader(output), reader: output, done: make(chan error, 1), cancel: stop, uuid: "server-uuid", connectionID: 42}
	go func() {
		defer func() { _ = input.Close(); _ = sink.Close() }() // fixture pipe cleanup
		scanner := bufio.NewScanner(input)
		for scanner.Scan() {
			if session.Err() != nil {
				h.done <- session.Err()
				return
			}
			if strings.HasPrefix(scanner.Text(), "SELECT 'envplane_unlock'") {
				if _, err := fmt.Fprintln(sink, "envplane_unlock\t42\tserver-uuid"); err != nil {
					h.done <- err
					return
				}
			}
			if scanner.Text() == "quit" {
				h.done <- nil
				return
			}
		}
		h.done <- ErrUnsafe
	}()
	cancel()
	release, stopRelease := context.WithTimeout(context.WithoutCancel(operation), time.Second)
	defer stopRelease()
	if err := h.Release(release); err != nil {
		t.Fatal("operation cancellation prevented verified release", err)
	}
}
