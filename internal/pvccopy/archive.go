package pvccopy

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
)

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(b)
}

func validPath(s string) bool {
	return s != "." && len(s) > 0 && len(s) <= 1024 && fs.ValidPath(s) && path.Clean(s) == s && !strings.ContainsAny(s, "\\\x00\r\n") && s != StateDirectory && !strings.HasPrefix(s, StateDirectory+"/")
}

func hashHeader(h hash.Hash, entry *tar.Header) {
	_ = binary.Write(h, binary.BigEndian, uint32(len(entry.Name)))
	_, _ = h.Write([]byte(entry.Name))
	_, _ = h.Write([]byte{entry.Typeflag})
	for _, value := range []int64{entry.Size, entry.Mode, int64(entry.Uid), int64(entry.Gid)} {
		_ = binary.Write(h, binary.BigEndian, value)
	}
}

// archive/tar accepts a raw EOF without the two end blocks. Require a complete
// native stream, so cancellation/truncation at an entry boundary is not success.
type archiveReader struct {
	r     io.Reader
	tail  [1024]byte
	count int64
}

func (r *archiveReader) Read(b []byte) (int, error) {
	n, err := r.r.Read(b)
	if n >= len(r.tail) {
		copy(r.tail[:], b[n-len(r.tail):n])
	} else if n > 0 {
		copy(r.tail[:], r.tail[n:])
		copy(r.tail[len(r.tail)-n:], b[:n])
	}
	r.count += int64(n)
	return n, err
}
func (r *archiveReader) complete() bool {
	if r.count < 1024 || r.count%512 != 0 {
		return false
	}
	for _, b := range r.tail {
		if b != 0 {
			return false
		}
	}
	return true
}

// Export emits a deliberately small tar subset: sorted directories and regular
// files only, preserving numeric ownership and regular mode bits, not links,
// extended metadata, sparse layout, or devices. Linux refuses visible xattrs.
// ignoreState is used only for verifying a claimed target, never for source copy.
func export(ctx context.Context, dir string, p Plan, w io.Writer, ignoreState bool) (Receipt, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return Receipt{}, err
	}
	defer root.Close()
	names := []string{"."}
	// WalkDir reads an entire directory before checking our entry bound. Batch
	// enumeration avoids unbounded allocation on hostile/oversized source trees.
	var walk func(string) error
	walk = func(dir string) error {
		f, err := root.Open(dir)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := checkUnsupportedMetadata(f); err != nil {
			return err
		}
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			entries, readErr := f.ReadDir(64)
			for _, de := range entries {
				name := de.Name()
				if dir != "." {
					name = dir + "/" + name
				}
				if ignoreState && name == StateDirectory {
					continue
				}
				if !validPath(name) || (!de.IsDir() && !de.Type().IsRegular()) {
					return fmt.Errorf("%w: unsupported source entry", ErrUnsafe)
				}
				if len(names) >= p.MaxEntries {
					return fmt.Errorf("%w: entry limit", ErrUnsafe)
				}
				names = append(names, name)
				if de.IsDir() {
					if err := walk(name); err != nil {
						return err
					}
				}
			}
			if readErr == io.EOF {
				return nil
			}
			if readErr != nil {
				return readErr
			}
		}
	}
	err = walk(".")
	if err != nil {
		return Receipt{}, err
	}
	sort.Strings(names[1:])
	tw := tar.NewWriter(w)
	h := sha256.New()
	r := Receipt{}
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return Receipt{}, err
		}
		st, err := root.Lstat(name)
		if err != nil {
			return Receipt{}, err
		}
		kind, size := byte(tar.TypeReg), st.Size()
		if st.IsDir() {
			kind, size = tar.TypeDir, 0
		} else if !st.Mode().IsRegular() || !singleLink(st) {
			return Receipt{}, fmt.Errorf("%w: nonregular source", ErrUnsafe)
		}
		if size < 0 || size > p.MaxBytes-r.Bytes {
			return Receipt{}, fmt.Errorf("%w: byte limit", ErrUnsafe)
		}
		uid, gid, ok := ownership(st)
		if !ok || st.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			return Receipt{}, fmt.Errorf("%w: unsupported source metadata", ErrUnsafe)
		}
		header := &tar.Header{Name: name, Typeflag: kind, Size: size, Mode: int64(st.Mode().Perm()), Uid: uid, Gid: gid, Format: tar.FormatUSTAR}
		// Inspect the same inode used for streaming. Directories were already
		// checked by the bounded walk; recheck before emitting their metadata.
		f, err := root.Open(name)
		if err != nil {
			return Receipt{}, err
		}
		actual, se := f.Stat()
		if se != nil || !os.SameFile(st, actual) {
			f.Close()
			return Receipt{}, fmt.Errorf("%w: source changed", ErrUnsafe)
		}
		if err := checkUnsupportedMetadata(f); err != nil {
			f.Close()
			return Receipt{}, err
		}
		if name == "." {
			r.Root = RootReceipt{UID: uid, GID: gid, Mode: uint32(header.Mode)}
		}
		if err := tw.WriteHeader(header); err != nil {
			f.Close()
			return Receipt{}, err
		}
		hashHeader(h, header)
		if kind == tar.TypeReg {
			if !actual.Mode().IsRegular() {
				f.Close()
				return Receipt{}, fmt.Errorf("%w: source changed", ErrUnsafe)
			}
			_, err = io.CopyN(io.MultiWriter(tw, h), contextReader{ctx, f}, size)
			var extra [1]byte
			n, end := f.Read(extra[:])
			attributeErr := checkUnsupportedMetadata(f)
			closeErr := f.Close()
			if err != nil {
				return Receipt{}, err
			}
			if n != 0 || end != io.EOF || closeErr != nil {
				return Receipt{}, fmt.Errorf("%w: source changed", ErrUnsafe)
			}
			if attributeErr != nil {
				return Receipt{}, attributeErr
			}
		} else {
			if err := f.Close(); err != nil {
				return Receipt{}, err
			}
		}
		r.Bytes += size
		r.Entries++
	}
	if err := tw.Close(); err != nil {
		return Receipt{}, err
	}
	r.SHA256 = hex.EncodeToString(h.Sum(nil))
	return r, nil
}

func Export(ctx context.Context, dir string, p Plan, w io.Writer) (Receipt, error) {
	if err := p.Validate(); err != nil {
		return Receipt{}, err
	}
	return export(ctx, dir, p, w, false)
}

// Relay validates and re-encodes the archive without buffering file data. The
// caller must close/cancel its transport to interrupt a blocked Read or Write.
func Relay(ctx context.Context, p Plan, input io.Reader, output io.Writer) (Receipt, error) {
	return consume(ctx, p, input, func(h *tar.Header) (io.WriteCloser, error) {
		return nil, nil
	}, output)
}

func consume(ctx context.Context, p Plan, input io.Reader, open func(*tar.Header) (io.WriteCloser, error), output io.Writer) (Receipt, error) {
	if err := p.Validate(); err != nil {
		return Receipt{}, err
	}
	// Bounds both file payload and tar metadata, including hidden PAX headers.
	wire := &archiveReader{r: contextReader{ctx, input}}
	limited := &io.LimitedReader{R: wire, N: p.MaxBytes + int64(p.MaxEntries)*2048 + 2048}
	tr := tar.NewReader(limited)
	var tw *tar.Writer
	if output != nil {
		tw = tar.NewWriter(output)
	}
	hash := sha256.New()
	receipt := Receipt{}
	last := ""
	dirs := map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Receipt{}, err
		}
		rootEntry := receipt.Entries == 0 && h.Name == "." && h.Typeflag == tar.TypeDir
		if (!rootEntry && (!validPath(h.Name) || h.Name <= last)) || (receipt.Entries == 0 && !rootEntry) || h.Format != tar.FormatUSTAR || h.Linkname != "" || len(h.PAXRecords) != 0 || h.Mode < 0 || h.Mode > 0777 || h.Uid < 0 || h.Gid < 0 || h.Uid > 1<<31-1 || h.Gid > 1<<31-1 || (h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir) || h.Size < 0 || (h.Typeflag == tar.TypeDir && h.Size != 0) {
			return Receipt{}, fmt.Errorf("%w: invalid archive entry", ErrUnsafe)
		}
		parent := path.Dir(h.Name)
		if parent != "." && !dirs[parent] {
			return Receipt{}, fmt.Errorf("%w: missing parent", ErrUnsafe)
		}
		if receipt.Entries >= p.MaxEntries || h.Size > p.MaxBytes-receipt.Bytes {
			return Receipt{}, fmt.Errorf("%w: archive limit", ErrUnsafe)
		}
		if !rootEntry {
			last = h.Name
		}
		if h.Typeflag == tar.TypeDir {
			dirs[h.Name] = true
		}
		clean := &tar.Header{Name: h.Name, Typeflag: h.Typeflag, Size: h.Size, Mode: h.Mode, Uid: h.Uid, Gid: h.Gid, Format: tar.FormatUSTAR}
		if rootEntry {
			receipt.Root = RootReceipt{UID: h.Uid, GID: h.Gid, Mode: uint32(h.Mode)}
		}
		if tw != nil {
			if err := tw.WriteHeader(clean); err != nil {
				return Receipt{}, err
			}
		}
		file, err := open(clean)
		if err != nil {
			return Receipt{}, err
		}
		hashHeader(hash, clean)
		writers := []io.Writer{hash}
		if tw != nil {
			writers = append(writers, tw)
		}
		if file != nil {
			writers = append(writers, file)
		}
		_, err = io.CopyN(io.MultiWriter(writers...), contextReader{ctx, tr}, h.Size)
		if file != nil {
			ce := file.Close()
			if err == nil {
				err = ce
			}
		}
		if err != nil {
			return Receipt{}, err
		}
		receipt.Bytes += h.Size
		receipt.Entries++
	}
	// Reject trailing data and truncated endings, not just tar's first EOF.
	var extra [1]byte
	n, err := limited.Read(extra[:])
	if limited.N <= 0 || n != 0 || err != io.EOF || !wire.complete() || receipt.Entries == 0 {
		return Receipt{}, fmt.Errorf("%w: trailing archive data or wire limit", ErrUnsafe)
	}
	if tw != nil {
		if err := tw.Close(); err != nil {
			return Receipt{}, err
		}
	}
	receipt.SHA256 = hex.EncodeToString(hash.Sum(nil))
	return receipt, nil
}

type syncedFile struct {
	*os.File
	Header *tar.Header
}

func (f syncedFile) Close() error {
	info, err := f.Stat()
	if err == nil {
		uid, gid, ok := ownership(info)
		if !ok {
			err = ErrUnsafe
		} else if uid != f.Header.Uid || gid != f.Header.Gid {
			err = f.Chown(f.Header.Uid, f.Header.Gid)
		}
	}
	if err == nil {
		err = f.Chmod(os.FileMode(f.Header.Mode))
	}
	if err == nil {
		err = f.Sync()
	}
	ce := f.File.Close()
	if err != nil {
		return err
	}
	return ce
}

// Paths use os.Root for containment, and exclusively created objects in a
// caller-fenced empty target. Symlinks are never an accepted archive entry.
type targetReceiver struct {
	root        *os.Root
	directories []*tar.Header
}

func (r *targetReceiver) open(h *tar.Header) (io.WriteCloser, error) {
	root := r.root
	if h.Typeflag == tar.TypeDir {
		r.directories = append(r.directories, h)
		if h.Name == "." {
			return nil, nil
		}
		return nil, root.Mkdir(h.Name, 0700)
	}
	f, err := root.OpenFile(h.Name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	return syncedFile{f, h}, nil
}
func (r *targetReceiver) finish() error {
	// Directories remain helper-writable until descendants are fully restored.
	for i := len(r.directories) - 1; i >= 0; i-- {
		h := r.directories[i]
		st, err := r.root.Lstat(h.Name)
		if err != nil {
			return err
		}
		uid, gid, ok := ownership(st)
		if !ok {
			return ErrUnsafe
		}
		if uid != h.Uid || gid != h.Gid {
			if err := r.root.Chown(h.Name, h.Uid, h.Gid); err != nil {
				return err
			}
		}
		if err := r.root.Chmod(h.Name, os.FileMode(h.Mode)); err != nil {
			return err
		}
	}
	return nil
}
