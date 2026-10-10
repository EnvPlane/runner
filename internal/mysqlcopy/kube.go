package mysqlcopy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/envplane/runner/internal/pvccopy"
)

// Endpoint identifies only an owned helper, NEVER the existing source Pod.
type Endpoint struct {
	Pod       Ref
	Container string
}

// ProofDriver is the lifecycle boundary. Implementations must re-read reviewed
// source PVC/workload/Service/Secret identities, validate exact helper manifests,
// hold the prepublication lease, reject all other target consumers, and use UID
// preconditions for helper deletion. A plan alone is not authorization.
// Source must ensure a new client-only helper (no source PVC mount), projected
// credentials, memory emptyDir configs, fixed images and verified TLS CA.
// Target must create-only a fresh native PVC/server, verify empty datadir before
// initialization, and refuse foreign/partial targets. It may never overwrite a
// completed target. Inspect must reverify persisted proof, not trust a marker.
type ProofDriver interface {
	Source(context.Context, Plan) (Endpoint, string, error) // helper + actual source Pod UID
	Target(context.Context, Plan) (Endpoint, TargetIdentity, error)
	Validate(context.Context, Plan, Endpoint, bool) error // readback UID/spec and target exclusivity
	Inspect(context.Context, Plan) (*Receipt, error)
	Shutdown(context.Context, Plan, Endpoint) error // clean MySQL exit verified, not force delete
	Commit(context.Context, Plan, Receipt) error    // immutable UID-bound receipt after shutdown
	Cleanup(context.Context, Plan) error
	Quiescence(context.Context, Plan) (Hold, error) // externally held scoped authority
}

// KubeTransport streams through kubectl exec. It has no Kubernetes Go dependency.
// These allowlists are operator configuration, NEVER copied from job flags.
// The instance is single-use/single-plan; main owns serialized lease dispatch.
type KubeTransport struct {
	Commands                pvccopy.CommandRunner
	Driver                  ProofDriver
	SourceNamespaces        []string
	RunnerImage, MySQLImage string
	source, target          Endpoint
}

func (k *KubeTransport) allowed(p Plan) error {
	if p.Validate() != nil || k.Driver == nil || !slices.Contains(k.SourceNamespaces, p.Source.PVC.Namespace) || k.RunnerImage != p.ConfigImage || k.MySQLImage != p.Source.Image {
		return ErrUnsafe
	}
	return nil
}
func (k *KubeTransport) run(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	r := k.Commands
	if r == nil {
		r = pvccopy.OSKubectl{}
	}
	return safeError(ctx, r.Run(ctx, args, in, out))
}

type limitBuffer struct {
	bytes.Buffer
	max int
}

func (b *limitBuffer) Write(p []byte) (int, error) {
	if len(p) > b.max-b.Len() {
		return 0, ErrUnsafe
	}
	return b.Buffer.Write(p)
}

func (k *KubeTransport) secret(ctx context.Context, r Ref) (map[string][]byte, error) {
	b := &limitBuffer{max: 1 << 20}
	if err := k.run(ctx, []string{"get", "secret", r.Name, "-n", r.Namespace, "-o", "json"}, nil, b); err != nil {
		return nil, err
	}
	var obj struct {
		Metadata struct {
			UID               string  `json:"uid"`
			DeletionTimestamp *string `json:"deletionTimestamp"`
		}
		Data map[string]string
	}
	if json.Unmarshal(b.Bytes(), &obj) != nil || obj.Metadata.UID != r.UID || obj.Metadata.DeletionTimestamp != nil {
		return nil, ErrUnsafe
	}
	data := map[string][]byte{}
	for key, v := range obj.Data {
		raw, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			clearSecrets(data)
			return nil, ErrUnsafe
		}
		data[key] = raw
	}
	return data, nil
}
func clearSecrets(data map[string][]byte) {
	for _, v := range data {
		clear(v)
	}
}
func (k *KubeTransport) credential(ctx context.Context, r CredentialRef, app bool) ([32]byte, [32]byte, error) {
	data, err := k.secret(ctx, r.Ref)
	if err != nil {
		return [32]byte{}, [32]byte{}, err
	}
	defer clearSecrets(data)
	u := r.Username
	if r.UsernameKey != "" {
		u = string(data[r.UsernameKey])
	}
	password := data[r.PasswordKey]
	if !identifier.MatchString(u) || (app && strings.EqualFold(u, "root")) || len(password) == 0 || len(password) > 4096 || bytes.IndexByte(password, 0) >= 0 {
		return [32]byte{}, [32]byte{}, ErrUnsafe
	}
	return valueHash(password), valueHash([]byte(u)), nil
}
func (k *KubeTransport) CheckCredentials(ctx context.Context, p Plan) (CredentialCheck, error) {
	if err := k.allowed(p); err != nil {
		return CredentialCheck{}, err
	}
	var r CredentialCheck
	var err error
	if r.Source, r.Principals[0], err = k.credential(ctx, p.Source.Credentials, true); err != nil {
		return r, err
	}
	if r.Target, r.Principals[1], err = k.credential(ctx, p.Target.Credentials, true); err != nil {
		return r, err
	}
	root := CredentialRef{Ref: p.Target.Credentials.Ref, Username: "root", PasswordKey: p.Target.RootPasswordKey}
	if r.TargetRoot, _, err = k.credential(ctx, root, false); err != nil {
		return r, err
	}
	if p.DDLMode == "backup_lock" {
		if r.Backup, r.Principals[2], err = k.credential(ctx, p.Source.BackupAdminSecretRef, false); err != nil {
			return r, err
		}
	}
	ca, err := k.secret(ctx, p.Source.TLSCASecret)
	if err != nil {
		return r, err
	}
	defer clearSecrets(ca)
	if len(ca[p.Source.TLSCAKey]) == 0 {
		return r, ErrUnsafe
	}
	r.CA = valueHash(ca[p.Source.TLSCAKey])
	return r, nil
}
func (k *KubeTransport) exec(ctx context.Context, p Plan, ep Endpoint, target bool, args []string, in io.Reader, out io.Writer) error {
	if k.allowed(p) != nil || !validRef(ep.Pod) || !name.MatchString(ep.Container) {
		return ErrUnsafe
	}
	ns := p.Source.PVC.Namespace
	if target {
		ns = p.Target.Namespace
	}
	if ep.Pod.Namespace != ns {
		return ErrUnsafe
	}
	if k.Driver.Validate(ctx, p, ep, target) != nil {
		return ErrUnsafe
	}
	argv := []string{"exec", "-n", ns, ep.Pod.Name, "-c", ep.Container, "-i", "--"}
	argv = append(argv, args...)
	return k.run(ctx, argv, in, out)
}
func clientArgs(p Plan, target, admin bool) []string {
	config := "/config/app.cnf"
	if admin {
		config = "/config/admin.cnf"
	}
	a := []string{"mysql", "--defaults-extra-file=" + config, "--binary-mode=1", "--local-infile=0", "--skip-reconnect", "--batch", "--skip-column-names", "--unbuffered", "--connect-timeout=10"}
	if target {
		return append(a, "--protocol=SOCKET", "--socket=/run/mysqld/mysqld.sock", "--database="+p.Target.Database)
	}
	return append(a, "--protocol=TCP", "--host="+p.Source.TLSServerName, "--port="+strconv.Itoa(p.Source.Port), "--ssl-mode=VERIFY_IDENTITY", "--ssl-ca=/credentials/ca.pem", "--database="+p.Source.Database)
}
func (k *KubeTransport) query(ctx context.Context, p Plan, sql string) (string, error) {
	return k.queryCredential(ctx, p, sql, false)
}
func (k *KubeTransport) queryCredential(ctx context.Context, p Plan, sql string, admin bool) (string, error) {
	b := &limitBuffer{max: 1 << 20}
	if err := k.exec(ctx, p, k.source, false, clientArgs(p, false, admin), strings.NewReader(sql), b); err != nil {
		return "", err
	}
	return b.String(), nil
}
func (k *KubeTransport) Source(ctx context.Context, p Plan) (SourceIdentity, error) {
	if k.allowed(p) != nil {
		return SourceIdentity{}, ErrUnsafe
	}
	ep, uid, err := k.Driver.Source(ctx, p)
	if err != nil {
		return SourceIdentity{}, ErrUnsafe
	}
	if k.source.Pod.UID != "" && k.source != ep {
		return SourceIdentity{}, ErrUnsafe
	}
	k.source = ep
	identity, err := k.query(ctx, p, "SELECT @@server_uuid, VERSION();\n")
	if err != nil {
		return SourceIdentity{}, err
	}
	cols := strings.Split(strings.TrimSpace(identity), "\t")
	if len(cols) != 2 {
		return SourceIdentity{}, ErrUnsafe
	}
	// Stored code/views and external schema dependencies are not transferable by
	// this deliberately small dump profile. Only metadata is returned to runner.
	db := p.Source.Database
	admin := p.DDLMode == "backup_lock"
	grants, err := k.queryCredential(ctx, p, "SHOW GRANTS FOR CURRENT_USER;\n", admin)
	if err != nil || !catalogAuthority(db, grants) {
		return SourceIdentity{}, ErrUnsafe
	}
	unsupported, err := k.queryCredential(ctx, p, fmt.Sprintf("SELECT (SELECT COUNT(*) FROM information_schema.tables WHERE table_schema='%s' AND table_type<>'BASE TABLE')+(SELECT COUNT(*) FROM information_schema.triggers WHERE trigger_schema='%s')+(SELECT COUNT(*) FROM information_schema.routines WHERE routine_schema='%s')+(SELECT COUNT(*) FROM information_schema.events WHERE event_schema='%s')+(SELECT COUNT(*) FROM information_schema.key_column_usage WHERE table_schema='%s' AND referenced_table_schema IS NOT NULL AND referenced_table_schema<>'%s');\n", db, db, db, db, db, db), admin)
	if err != nil || strings.TrimSpace(unsupported) != "0" {
		return SourceIdentity{}, ErrUnsafe
	}
	inventory, err := k.query(ctx, p, fmt.Sprintf("SELECT t.table_name,t.engine,EXISTS(SELECT 1 FROM information_schema.table_constraints c WHERE c.table_schema=t.table_schema AND c.table_name=t.table_name AND c.constraint_type='PRIMARY KEY') FROM information_schema.tables t WHERE t.table_schema='%s' ORDER BY t.table_name;\n", db))
	if err != nil {
		return SourceIdentity{}, err
	}
	fullInventory, err := k.queryCredential(ctx, p, fmt.Sprintf("SELECT t.table_name,t.engine,EXISTS(SELECT 1 FROM information_schema.table_constraints c WHERE c.table_schema=t.table_schema AND c.table_name=t.table_name AND c.constraint_type='PRIMARY KEY') FROM information_schema.tables t WHERE t.table_schema='%s' ORDER BY t.table_name;\n", db), admin)
	if err != nil || inventory != fullInventory {
		return SourceIdentity{}, ErrUnsafe
	}
	s := SourceIdentity{PodUID: uid, ServerUUID: cols[0], Version: cols[1]}
	for _, line := range strings.Split(strings.TrimSpace(inventory), "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 3 || (f[2] != "0" && f[2] != "1") {
			return s, ErrUnsafe
		}
		s.Tables = append(s.Tables, Table{f[0], f[1], f[2] == "1"})
	}
	if err := validateSource(p, s); err != nil {
		return s, err
	}
	if driver, ok := k.Driver.(interface{ observeSource(SourceIdentity) error }); ok {
		if err := driver.observeSource(s); err != nil {
			return s, err
		}
	}
	return s, nil
}
func dumpArgs(p Plan, target bool) []string {
	a := clientArgs(p, target, false)
	a[0] = "mysqldump"
	// mysql-only switches are replaced with the exact deterministic dump profile.
	a = slices.DeleteFunc(a, func(v string) bool {
		return v == "--batch" || v == "--binary-mode=1" || v == "--local-infile=0" || v == "--skip-reconnect" || v == "--skip-column-names" || v == "--unbuffered" || strings.HasPrefix(v, "--connect-timeout=") || strings.HasPrefix(v, "--database=")
	})
	a = append(a, "--single-transaction", "--skip-lock-tables", "--compact", "--skip-add-drop-table", "--skip-add-locks", "--skip-disable-keys", "--skip-extended-insert", "--complete-insert", "--order-by-primary", "--hex-blob", "--skip-triggers", "--set-gtid-purged=OFF", "--no-tablespaces", "--column-statistics=0", "--default-character-set=utf8mb4", p.Source.Database)
	return a
}
func (k *KubeTransport) Dump(ctx context.Context, p Plan, w io.Writer) error {
	return k.exec(ctx, p, k.source, false, dumpArgs(p, false), nil, w)
}
func (k *KubeTransport) Restore(ctx context.Context, p Plan, id TargetIdentity, r io.Reader) error {
	if k.target.Pod.UID != id.PodUID {
		return ErrUnsafe
	}
	// Referential constraints are checked via exact logical proof, not restore
	// order. Only the temporary target connection is affected by session SETs.
	in := io.MultiReader(strings.NewReader("SET SESSION SQL_MODE='NO_AUTO_VALUE_ON_ZERO'; SET SESSION TIME_ZONE='+00:00'; SET SESSION FOREIGN_KEY_CHECKS=0;\n"), r, strings.NewReader("\nSET SESSION FOREIGN_KEY_CHECKS=1;\n"))
	return k.exec(ctx, p, k.target, true, clientArgs(p, true, false), in, io.Discard)
}
func (k *KubeTransport) TargetDump(ctx context.Context, p Plan, id TargetIdentity, w io.Writer) error {
	if k.target.Pod.UID != id.PodUID {
		return ErrUnsafe
	}
	return k.exec(ctx, p, k.target, true, dumpArgs(p, true), nil, w)
}
func (k *KubeTransport) Inspect(ctx context.Context, p Plan) (*Receipt, error) {
	if k.allowed(p) != nil {
		return nil, ErrUnsafe
	}
	return k.Driver.Inspect(ctx, p)
}
func (k *KubeTransport) Provision(ctx context.Context, p Plan) (TargetIdentity, error) {
	if k.allowed(p) != nil {
		return TargetIdentity{}, ErrUnsafe
	}
	ep, id, err := k.Driver.Target(ctx, p)
	if err != nil {
		return id, ErrUnsafe
	}
	if ep.Pod.UID != id.PodUID || ep.Pod.Namespace != p.Target.Namespace {
		return id, ErrUnsafe
	}
	k.target = ep
	return id, k.Driver.Validate(ctx, p, ep, true)
}
func (k *KubeTransport) Shutdown(ctx context.Context, p Plan, id TargetIdentity) error {
	if k.target.Pod.UID != id.PodUID || k.Driver.Validate(ctx, p, k.target, true) != nil {
		return ErrUnsafe
	}
	return k.Driver.Shutdown(ctx, p, k.target)
}
func (k *KubeTransport) Commit(ctx context.Context, p Plan, r Receipt) error {
	return k.Driver.Commit(ctx, p, r)
}
func (k *KubeTransport) Cleanup(ctx context.Context, p Plan) error {
	if k.Driver == nil {
		return nil
	}
	return k.Driver.Cleanup(ctx, p)
}

// backupHold owns the actual mysql process/connection. Check uses an on-session
// challenge, so a process merely remaining alive is not accepted as lock proof.
type backupHold struct {
	mu           sync.Mutex
	in           *io.PipeWriter
	out          *bufio.Reader
	reader       *io.PipeReader
	done         chan error
	cancel       context.CancelFunc
	uuid         string
	sequence     int
	closed       bool
	connectionID uint64
}

func (k *KubeTransport) HoldDDL(ctx context.Context, p Plan) (Hold, error) {
	if k.allowed(p) != nil {
		return nil, ErrUnsafe
	}
	if p.DDLMode == "quiescence" {
		return k.Driver.Quiescence(ctx, p)
	}
	identity, err := k.query(ctx, p, "SELECT @@server_uuid;\n")
	if err != nil {
		return nil, err
	}
	session, cancel := context.WithCancel(ctx)
	input, writer := io.Pipe()
	output, sink := io.Pipe()
	h := &backupHold{in: writer, out: bufio.NewReaderSize(output, 4096), reader: output, done: make(chan error, 1), cancel: cancel, uuid: strings.TrimSpace(identity)}
	go func() {
		err := k.exec(session, p, k.source, false, clientArgs(p, false, true), input, sink)
		_ = input.CloseWithError(err)
		_ = sink.CloseWithError(err)
		h.done <- err
	}() // pipe cleanup; process error retained in done
	stop := context.AfterFunc(session, func() { _ = writer.CloseWithError(session.Err()); _ = output.CloseWithError(session.Err()) })
	_ = stop // callback lives exactly as long as session
	if _, err = io.WriteString(writer, "LOCK INSTANCE FOR BACKUP;\n"); err == nil {
		err = h.Check(ctx)
	}
	if err != nil {
		cancel()
		_ = writer.Close()
		_ = output.Close()
		return nil, ErrUnsafe
	}
	return h, nil
}
func (h *backupHold) Check(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed || ctx.Err() != nil {
		return ErrUnsafe
	}
	select {
	case <-h.done:
		return ErrUnsafe
	default:
	}
	h.sequence++
	marker := fmt.Sprintf("envplane_lock_%d", h.sequence)
	stop := context.AfterFunc(ctx, func() { h.cancel() })
	defer stop()
	if _, err := io.WriteString(h.in, fmt.Sprintf("SELECT '%s', CONNECTION_ID(), @@server_uuid;\n", marker)); err != nil {
		return ErrUnsafe
	}
	line, err := h.out.ReadSlice('\n')
	if err != nil {
		return ErrUnsafe
	}
	f := strings.Split(strings.TrimSpace(string(line)), "\t")
	if len(f) != 3 || f[0] != marker || f[2] != h.uuid {
		return ErrUnsafe
	}
	id, err := strconv.ParseUint(f[1], 10, 64)
	if err != nil || id == 0 {
		return ErrUnsafe
	}
	if h.connectionID != 0 && h.connectionID != id {
		return ErrUnsafe
	}
	h.connectionID = id
	return nil
}
func (h *backupHold) Release(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return ErrUnsafe
	}
	h.closed = true
	// Closing stdin exits the connection, releasing its instance backup lock.
	closeErr := h.in.Close()
	defer h.cancel()
	defer func() { _ = h.reader.Close() }() // output pipe cleanup, not a durable writer
	select {
	case err := <-h.done:
		if closeErr != nil || err != nil {
			return ErrOperation
		}
		return nil
	case <-ctx.Done():
		h.cancel()
		return ctx.Err()
	}
}
