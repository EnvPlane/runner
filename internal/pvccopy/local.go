package pvccopy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

func readJSON(root *os.Root, name string, value any) error {
	st, err := root.Lstat(name)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Size() > 16384 {
		return ErrPartial
	}
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 16385))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return ErrPartial
	}
	return nil
}

func writeJSON(root *os.Root, name string, value any) error {
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	err = json.NewEncoder(f).Encode(value)
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err != nil {
		return err
	}
	return ce
}

func syncDirectory(root *os.Root, name string) error {
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// Inspect returns nil only for a genuinely empty target. Completed targets are
// rehashed; any partial, malformed, mismatched, or tampered state fails closed.
func Inspect(ctx context.Context, dir string, p Plan) (*Marker, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	f, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	if err := checkUnsupportedMetadata(f); err != nil {
		f.Close()
		return nil, err
	}
	entries, err := f.ReadDir(1)
	f.Close()
	if err == io.EOF && len(entries) == 0 {
		return nil, nil
	}
	if err != nil && err != io.EOF {
		return nil, err
	}
	st, err := root.Lstat(StateDirectory)
	if err != nil || !st.IsDir() {
		return nil, ErrPartial
	}
	var claim Marker
	if readJSON(root, StateDirectory+"/claim.json", &claim) != nil || claim != markerFor(p, Receipt{}) {
		return nil, ErrPartial
	}
	var marker Marker
	if readJSON(root, StateDirectory+"/complete.json", &marker) != nil || marker != markerFor(p, marker.Receipt) {
		return nil, ErrPartial
	}
	state, err := root.Open(StateDirectory)
	if err != nil {
		return nil, err
	}
	names, err := state.Readdirnames(-1)
	state.Close()
	if err != nil || len(names) != 3 {
		return nil, ErrPartial
	}
	var received Receipt
	if readJSON(root, StateDirectory+"/received.json", &received) != nil || received != marker.Receipt {
		return nil, ErrPartial
	}
	r, err := export(ctx, dir, p, io.Discard, true)
	if err != nil {
		return nil, err
	}
	if r != marker.Receipt {
		return nil, ErrPartial
	}
	return &marker, nil
}

// Receive takes an exclusive durable claim before writing payload. It never
// removes a claim or payload on error: unknown outcomes cannot be retried over.
func Receive(ctx context.Context, dir string, p Plan, input io.Reader) (Receipt, error) {
	if err := p.Validate(); err != nil {
		return Receipt{}, err
	}
	m, err := Inspect(ctx, dir, p)
	if err != nil {
		return Receipt{}, err
	}
	if m != nil {
		return Receipt{}, ErrPartial
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return Receipt{}, err
	}
	defer root.Close()
	if err := root.Mkdir(StateDirectory, 0700); err != nil {
		return Receipt{}, fmt.Errorf("%w: exclusive claim", ErrPartial)
	}
	if err := writeJSON(root, StateDirectory+"/claim.json", markerFor(p, Receipt{})); err != nil {
		return Receipt{}, err
	}
	if err := syncDirectory(root, StateDirectory); err != nil {
		return Receipt{}, err
	}
	if err := syncDirectory(root, "."); err != nil {
		return Receipt{}, err
	}
	receiver := &targetReceiver{root: root}
	r, err := consume(ctx, p, input, receiver.open, nil)
	if err != nil {
		return Receipt{}, err
	}
	if err := receiver.finish(); err != nil {
		return Receipt{}, err
	}
	actual, err := export(ctx, dir, p, io.Discard, true)
	if err != nil {
		return Receipt{}, err
	}
	if actual != r {
		return Receipt{}, fmt.Errorf("%w: target verification mismatch", ErrUnsafe)
	}
	if err := writeJSON(root, StateDirectory+"/received.json", r); err != nil {
		return Receipt{}, err
	}
	if err := syncDirectory(root, StateDirectory); err != nil {
		return Receipt{}, err
	}
	return r, nil
}

// Complete requires both the runner's verified receipt and the helper's durable
// received receipt. The final marker is exclusively created and fsynced only
// after re-reading payload. It is never overwritten, even on a retry.
func Complete(ctx context.Context, dir string, p Plan, expected Receipt) (Marker, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return Marker{}, err
	}
	defer root.Close()
	if err := p.Validate(); err != nil {
		return Marker{}, err
	}
	var claim Marker
	if readJSON(root, StateDirectory+"/claim.json", &claim) != nil || claim != markerFor(p, Receipt{}) {
		return Marker{}, ErrPartial
	}
	var received Receipt
	if readJSON(root, StateDirectory+"/received.json", &received) != nil || received != expected {
		return Marker{}, ErrPartial
	}
	actual, err := export(ctx, dir, p, io.Discard, true)
	if err != nil {
		return Marker{}, err
	}
	if actual != expected {
		return Marker{}, ErrPartial
	}
	// Sync every payload directory before publishing completion.
	if err := syncTree(ctx, root, "."); err != nil {
		return Marker{}, err
	}
	marker := markerFor(p, actual)
	if err := writeJSON(root, StateDirectory+"/complete.json", marker); err != nil {
		return Marker{}, err
	}
	// received.json is retained as an audit receipt, including after crash.
	if err := syncDirectory(root, StateDirectory); err != nil {
		return Marker{}, err
	}
	if err := syncDirectory(root, "."); err != nil {
		return Marker{}, err
	}
	return marker, nil
}

func syncTree(ctx context.Context, root *os.Root, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := root.Open(name)
	if err != nil {
		return err
	}
	entries, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			child := entry.Name()
			if name != "." {
				child = name + "/" + child
			}
			if err := syncTree(ctx, root, child); err != nil {
				return err
			}
		}
	}
	return syncDirectory(root, name)
}

// CopyLocal exercises exactly the same archive, exclusive claim, and completion
// protocol on real directories. The caller guarantees offline exclusive access.
func CopyLocal(ctx context.Context, source, target string, p Plan) (Marker, error) {
	if err := p.Validate(); err != nil {
		return Marker{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	if sourceInfo, err := os.Stat(source); err == nil {
		targetInfo, targetErr := os.Stat(target)
		if targetErr != nil {
			return Marker{}, targetErr
		}
		if os.SameFile(sourceInfo, targetInfo) {
			return Marker{}, ErrUnsafe
		}
	}
	if m, err := Inspect(ctx, target, p); err != nil {
		return Marker{}, err
	} else if m != nil {
		return *m, nil
	}
	reader, writer := io.Pipe()
	defer reader.Close()
	done := make(chan error, 1)
	go func() { _, err := Export(ctx, source, p, writer); writer.CloseWithError(err); done <- err }()
	stop := context.AfterFunc(ctx, func() { reader.CloseWithError(ctx.Err()); writer.CloseWithError(ctx.Err()) })
	defer stop()
	r, err := Receive(ctx, target, p, reader)
	reader.CloseWithError(err)
	sourceErr := <-done
	if err != nil {
		return Marker{}, err
	}
	if sourceErr != nil {
		return Marker{}, sourceErr
	}
	return Complete(ctx, target, p, r)
}
