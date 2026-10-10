package mysqlcopy

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestCredentialOptionsRedactionAndEscaping(t *testing.T) {
	b, err := ClientOptions("app", "quote\"back\\line\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `password="quote\"back\\line\n"`) {
		t.Fatal("credential escaping changed")
	}
	for _, p := range []string{"", strings.Repeat("a", 4097), "contains\x00nul"} {
		if _, err := ClientOptions("app", p); err == nil {
			t.Fatal("unsupported credential accepted")
		}
	}
}
func TestTargetClaimNeverOverwrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foreign")
	if err := os.WriteFile(path, []byte("actual foreign data"), 0600); err != nil {
		t.Fatal(err)
	}
	if targetClaim(dir, strings.Repeat("a", 64), "target-uid", false) == nil {
		t.Fatal("foreign target overwritten")
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "actual foreign data" {
		t.Fatal("foreign bytes changed")
	}
}
func TestTargetClaimOwnershipAndUID(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("target-only root ownership requires disposable Linux root helper")
	}
	dir := t.TempDir()
	digest := strings.Repeat("a", 64)
	if err := targetClaim(dir, digest, "target-uid", false); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, TargetDataSubPath))
	if err != nil || st.Mode().Perm() != 0700 {
		t.Fatal("datadir permissions")
	}
	metadata, ok := st.Sys().(*syscall.Stat_t)
	if !ok || metadata.Uid != 999 || metadata.Gid != 999 {
		t.Fatal("datadir ownership")
	}
	if targetClaim(dir, digest, "target-uid", false) == nil {
		t.Fatal("partial target retry permitted")
	}
	if targetClaim(dir, digest, "foreign-uid", true) == nil {
		t.Fatal("foreign UID resumed")
	}
	if err := targetClaim(dir, digest, "target-uid", true); err != nil {
		t.Fatal(err)
	}
}
func TestTargetCredentialsSeparateRoot(t *testing.T) {
	p := testPlan()
	spec := helperSpec(p, "default", true, false, "target-uid")
	container := asMap(clone(spec)["containers"].([]any)[0])
	keys := map[string]string{}
	for _, v := range arr(container, "env") {
		e := asMap(v)
		keys[str(e, "name")] = str(e, "valueFrom", "secretKeyRef", "key")
	}
	if keys["MYSQL_ROOT_PASSWORD"] != p.Target.RootPasswordKey || keys["MYSQL_ROOT_PASSWORD"] == keys["MYSQL_PASSWORD"] || keys["MYSQL_RANDOM_ROOT_PASSWORD"] != "" {
		t.Fatal("unsafe root credential generation")
	}
	p.Target.RootPasswordKey = p.Target.Credentials.PasswordKey
	if p.Validate() == nil {
		t.Fatal("same root/app key accepted")
	}
}
func TestDumpArgumentsSupportAndIsolation(t *testing.T) {
	p := testPlan()
	for _, v := range dumpArgs(p, false) {
		if strings.Contains(v, "connect-timeout") || strings.Contains(v, "password=") {
			t.Fatal("unsupported option or password argv")
		}
	}
	joined := strings.Join(dumpArgs(p, false), " ")
	for _, required := range []string{"--single-transaction", "--skip-lock-tables", "--ssl-mode=VERIFY_IDENTITY", "--ssl-ca=/credentials/ca.pem", "--no-tablespaces"} {
		if !strings.Contains(joined, required) {
			t.Fatal("missing dump invariant", required)
		}
	}
}
