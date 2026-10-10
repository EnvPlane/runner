package mysqlcopy

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// HelperCLI is dispatched by main as mysql-client-config. All args are metadata
// paths/user names, NEVER passwords. /config MUST be a memory-backed emptyDir;
// password inputs MUST be exact projected approved Secret keys. This command
// creates only owned O_EXCL files, never reads database files or logs values.
// args: username-or-@usernamefile passwordfile outputfile (repeat triplets).
func HelperCLI(ctx context.Context, args []string) error {
	if len(args) > 0 && (strings.HasPrefix(args[0], "--target-claim=") || strings.HasPrefix(args[0], "--target-resume=")) {
		if len(args) < 2 || !strings.HasPrefix(args[1], "--target-uid=") {
			return ErrUnsafe
		}
		resume := strings.HasPrefix(args[0], "--target-resume=")
		digest := strings.SplitN(args[0], "=", 2)[1]
		uid := strings.TrimPrefix(args[1], "--target-uid=")
		if err := targetClaim("/data", digest, uid, resume); err != nil {
			return err
		}
		args = args[2:]
	}
	if len(args) == 0 || len(args)%3 != 0 || len(args) > 6 {
		return ErrUnsafe
	}
	for i := 0; i < len(args); i += 3 {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		user := args[i]
		if strings.HasPrefix(user, "@") {
			b, err := readCredential(strings.TrimPrefix(user, "@"))
			if err != nil {
				return ErrUnsafe
			}
			user = string(b)
		}
		password, err := readCredential(args[i+1])
		if err != nil {
			return ErrUnsafe
		}
		out := args[i+2]
		if filepath.Dir(out) != "/config" || !identifier.MatchString(user) || strings.ContainsAny(out, "\x00\r\n") {
			return ErrUnsafe
		}
		config, err := ClientOptions(user, string(password))
		clear(password)
		if err != nil {
			return err
		}
		f, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return ErrOperation
		}
		_, writeErr := f.Write(config)
		clear(config)
		closeErr := f.Close()
		if writeErr != nil || closeErr != nil {
			return ErrOperation
		}
		if err := os.Chown(out, 999, 999); err != nil {
			return ErrOperation
		}
	}
	for _, dir := range []string{"/tmp", "/run/mysqld"} {
		if err := os.Chown(dir, 999, 999); err != nil {
			return ErrOperation
		}
	}
	return nil
}

// targetClaim is called only on the target PVC, before MySQL's entrypoint. A
// fresh target must be wholly empty, not even an unexplained lost+found. The
// durable claim remains on every failure; retries never overwrite partial data.
func targetClaim(dir, digest, uid string, resume bool) error {
	if len(digest) != 64 || len(uid) < 1 || len(uid) > 256 {
		return ErrUnsafe
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return ErrUnsafe
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return ErrOperation
	}
	defer func() { _ = root.Close() }() // containment handle only
	record := digest + "\n" + uid + "\n"
	if resume {
		st, err := root.Lstat(".mysqlcopy-claim")
		if err != nil || !st.Mode().IsRegular() || st.Size() > 512 {
			return ErrPartial
		}
		b, err := root.ReadFile(".mysqlcopy-claim")
		if err != nil || string(b) != record {
			return ErrPartial
		}
		st, err = root.Lstat("mysql")
		if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return ErrPartial
		}
		return nil
	}
	directory, err := root.Open(".")
	if err != nil {
		return ErrOperation
	}
	entries, readErr := directory.ReadDir(1)
	_ = directory.Close() // directory enumeration only
	if readErr != io.EOF || len(entries) != 0 {
		return ErrPartial
	}
	f, err := root.OpenFile(".mysqlcopy-claim", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return ErrPartial
	}
	_, writeErr := io.WriteString(f, record)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return ErrOperation
	}
	if root.Mkdir("mysql", 0700) != nil {
		return ErrOperation
	}
	if root.Chown("mysql", 999, 999) != nil {
		return ErrOperation
	}
	directory, err = root.Open(".")
	if err != nil {
		return ErrOperation
	}
	syncErr = directory.Sync()
	closeErr = directory.Close()
	if syncErr != nil || closeErr != nil {
		return ErrOperation
	}
	return nil
}
func readCredential(path string) ([]byte, error) {
	if !strings.HasPrefix(path, "/credentials/") || filepath.Clean(path) != path || strings.ContainsAny(path, "\x00\r\n") {
		return nil, ErrUnsafe
	}
	st, err := os.Stat(path)
	if err != nil || st.Size() <= 0 || st.Size() > 4096 {
		return nil, ErrUnsafe
	}
	return os.ReadFile(path)
}
func ClientOptions(user, password string) ([]byte, error) {
	if !identifier.MatchString(user) || len(password) < 1 || len(password) > 4096 || strings.ContainsRune(password, 0) {
		return nil, ErrUnsafe
	}
	escape := func(s string) string {
		return strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\n", "\\n", "\r", "\\r", "\t", "\\t").Replace(s)
	}
	return []byte(fmt.Sprintf("[client]\nuser=\"%s\"\npassword=\"%s\"\n", escape(user), escape(password))), nil
}
