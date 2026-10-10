package mysqlcopy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func fixturePlan() Plan {
	pin := "mysql@sha256:" + strings.Repeat("a", 64)
	return Plan{Binding: "job/item", Tenant: "default", Project: "project", Environment: "preview", EnvironmentCreatedAt: time.Unix(42, 0).UTC(), DomainPlanDigest: strings.Repeat("b", 64), SecretMaterializationPlanDigest: strings.Repeat("c", 64), Source: Source{PVC: Ref{"source", "data", "pvc-source"}, StatefulSet: Ref{"source", "mysql", "sts-source"}, Container: "mysql", Image: pin, Service: "mysql", ServiceUID: "service-source", Database: "app", Port: 3306, Credentials: CredentialRef{Ref: Ref{"source", "app", "secret-source"}, Username: "backupreader", PasswordKey: "DB_PASSWORD"}, BackupAdminSecretRef: CredentialRef{Ref: Ref{"source", "backup", "secret-backup"}, Username: "backupadmin", PasswordKey: "PASSWORD"}, TLSCASecret: Ref{"source", "ca", "secret-ca"}, TLSCAKey: "ca.crt", TLSServerName: "mysql.source.svc"}, Target: Target{Namespace: "preview", PVCName: "mysql-data", StorageClass: "local-path", Image: pin, Database: "app", RequestedBytes: 1 << 20, Credentials: CredentialRef{Ref: Ref{"preview", "generated", "secret-target"}, Username: "app", PasswordKey: "DB_PASSWORD"}}, ConfigImage: "runner@sha256:" + strings.Repeat("d", 64), DDLMode: "backup_lock", MaxBytes: 1 << 20, MaxStatementBytes: 16 << 10, MaxTables: 10, MaxRows: 100, Timeout: 3 * time.Second}
}
func testPlan() Plan {
	p := fixturePlan()
	p.Target.RootPasswordKey = "MYSQL_ROOT_PASSWORD"
	p.DomainPlanDigest = "sha256:" + p.DomainPlanDigest
	p.SecretMaterializationPlanDigest = "sha256:" + p.SecretMaterializationPlanDigest
	return p
}

const fixtureDump = "/*!40101 SET @saved_cs_client = @@character_set_client */;\n/*!50503 SET character_set_client = utf8mb4 */;\nCREATE TABLE `records` (\n  `id` int NOT NULL,\n  `payload` varchar(128) DEFAULT NULL,\n  PRIMARY KEY (`id`)\n) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4;\n/*!40101 SET character_set_client = @saved_cs_client */;\nINSERT INTO `records` (`id`, `payload`) VALUES (1,'actual bytes; quote \\' and newline\\n');\n"

type fakeHold struct {
	lost     bool
	released bool
}

func (h *fakeHold) Check(context.Context) error {
	if h.lost {
		return ErrUnsafe
	}
	return nil
}
func (h *fakeHold) Release(context.Context) error { h.released = true; return nil }

type memoryTransport struct {
	dump, restored                                    []byte
	credentials                                       CredentialCheck
	source                                            SourceIdentity
	hold                                              fakeHold
	provisioned, shutdown, committed, cleaned         bool
	corrupt, partial, failDump, failRestore, earlyEOF bool
	marker                                            *Receipt
}

func newMemory() *memoryTransport {
	return &memoryTransport{dump: []byte(fixtureDump), credentials: CredentialCheck{Source: valueHash([]byte("sourceonly")), Target: valueHash([]byte("differenttarget")), TargetRoot: valueHash([]byte("independent-root"))}, source: SourceIdentity{PodUID: "actual-source-pod", ServerUUID: "actual-uuid", Version: "8.0.46", Tables: []Table{{"records", "InnoDB", true}}}}
}
func (m *memoryTransport) CheckCredentials(context.Context, Plan) (CredentialCheck, error) {
	return m.credentials, nil
}
func (m *memoryTransport) Source(context.Context, Plan) (SourceIdentity, error) { return m.source, nil }
func (m *memoryTransport) HoldDDL(context.Context, Plan) (Hold, error)          { return &m.hold, nil }
func (m *memoryTransport) Inspect(context.Context, Plan) (*Receipt, error) {
	if m.partial {
		return nil, ErrPartial
	}
	return m.marker, nil
}
func (m *memoryTransport) Provision(context.Context, Plan) (TargetIdentity, error) {
	m.provisioned = true
	return TargetIdentity{"pvc-target", "pod-target", "secret-target"}, nil
}
func (m *memoryTransport) Dump(ctx context.Context, p Plan, w io.Writer) error {
	if m.failDump {
		return errors.New("password=do-not-log")
	}
	_, err := w.Write(m.dump)
	return err
}
func (m *memoryTransport) Restore(ctx context.Context, p Plan, id TargetIdentity, r io.Reader) error {
	if m.failRestore {
		return errors.New("password=do-not-log")
	}
	if m.earlyEOF {
		b := make([]byte, 10)
		_, err := r.Read(b)
		return err
	}
	b, err := io.ReadAll(r)
	m.restored = b
	return err
}
func (m *memoryTransport) TargetDump(ctx context.Context, p Plan, id TargetIdentity, w io.Writer) error {
	b := m.restored
	if m.corrupt {
		b = bytes.ReplaceAll(b, []byte("actual bytes"), []byte("wrong bytes"))
	}
	_, err := w.Write(b)
	return err
}
func (m *memoryTransport) Shutdown(context.Context, Plan, TargetIdentity) error {
	m.shutdown = true
	return nil
}
func (m *memoryTransport) Commit(ctx context.Context, p Plan, r Receipt) error {
	if !m.shutdown {
		return ErrUnsafe
	}
	m.committed = true
	m.marker = &r
	return nil
}
func (m *memoryTransport) Cleanup(context.Context, Plan) error { m.cleaned = true; return nil }
func TestExecuteActualDataAndReceipt(t *testing.T) {
	p := testPlan()
	m := newMemory()
	e := DomainExecutor{m, func(context.Context, Plan, Stage) error { return nil }}
	r, err := e.ExecuteDomain(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(m.dump, m.restored) || !m.committed || !m.shutdown || !m.cleaned || !m.hold.released {
		t.Fatal("data or lifecycle incomplete")
	}
	if r.Proof.Bytes != int64(len(fixtureDump)) || r.Proof.SHA256 != hashString(m.dump) || r.Proof.Tables[0].Rows != 1 || r.Target.PVCUID != "pvc-target" || r.SourcePVCUID != "pvc-source" {
		t.Fatal("unbound proof")
	}
	again, err := e.ExecuteDomain(context.Background(), p)
	if err != nil || again.PlanDigest != r.PlanDigest {
		t.Fatal("idempotent receipt refused", err)
	}
}
func TestRefusals(t *testing.T) {
	for _, tc := range []struct {
		name        string
		change      func(*memoryTransport)
		beforeWrite bool
	}{
		{"same-password", func(m *memoryTransport) { m.credentials.Target = m.credentials.Source }, true},
		{"same-root-password", func(m *memoryTransport) { m.credentials.TargetRoot = m.credentials.Target }, true},
		{"nontransactional", func(m *memoryTransport) { m.source.Tables[0].Engine = "MyISAM" }, true},
		{"missing-primary-key", func(m *memoryTransport) { m.source.Tables[0].PrimaryKey = false }, true},
		{"lost-DDL-lock", func(m *memoryTransport) { m.hold.lost = true }, true},
		{"partial-target", func(m *memoryTransport) { m.partial = true }, true},
		{"corrupt-target", func(m *memoryTransport) { m.corrupt = true }, false},
		{"dump-failure", func(m *memoryTransport) { m.failDump = true }, false},
		{"restore-failure", func(m *memoryTransport) { m.failRestore = true }, false},
		{"early-EOF", func(m *memoryTransport) { m.earlyEOF = true }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMemory()
			tc.change(m)
			_, err := (DomainExecutor{m, func(context.Context, Plan, Stage) error { return nil }}).ExecuteDomain(context.Background(), testPlan())
			if err == nil || strings.Contains(err.Error(), "do-not-log") || m.committed {
				t.Fatal("unsafe success or secret exposure")
			}
			if tc.beforeWrite && m.provisioned {
				t.Fatal("target write before source refusal")
			}
		})
	}
}
func TestAuthorityAndCancellation(t *testing.T) {
	for _, stage := range []Stage{ReadSource, HoldDDL, ProvisionTarget, Transfer, Verify, Complete} {
		t.Run(string(stage), func(t *testing.T) {
			m := newMemory()
			_, err := (DomainExecutor{m, func(ctx context.Context, p Plan, s Stage) error {
				if s == stage {
					return ErrUnsafe
				}
				return nil
			}}).ExecuteDomain(context.Background(), testPlan())
			if err == nil || m.committed {
				t.Fatal("authority bypass")
			}
			if stage == ReadSource || stage == HoldDDL || stage == ProvisionTarget {
				if m.provisioned {
					t.Fatal("early target write")
				}
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (DomainExecutor{newMemory(), func(context.Context, Plan, Stage) error { return nil }}).ExecuteDomain(ctx, testPlan())
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestStreamSafety(t *testing.T) {
	for _, dump := range []string{"GRANT ALL ON *.* TO 'root';", "SET GLOBAL local_infile=1;", "/*!40101 SET @x=LOAD_FILE('/secret') */;", strings.ReplaceAll(fixtureDump, "ENGINE=InnoDB", "ENGINE=MyISAM"), fixtureDump + "DROP TABLE `records`;", strings.ReplaceAll(fixtureDump, "(1,'actual", "(1,'actual") + "SELECT 1;", "CREATE TABLE `records` (id int) ENGINE=InnoDB DATA DIRECTORY='/outside';"} {
		tr := newTracker(testPlan())
		_, err := tr.Write([]byte(dump))
		if err == nil {
			_, err = tr.Finish()
		}
		if err == nil {
			t.Fatal("accepted unsafe SQL")
		}
	}
	p := testPlan()
	p.MaxBytes = 10
	tr := newTracker(p)
	if _, err := tr.Write([]byte(fixtureDump)); err == nil {
		t.Fatal("byte bound")
	}
	p = testPlan()
	p.MaxStatementBytes = 50
	tr = newTracker(p)
	if _, err := tr.Write([]byte(fixtureDump)); err == nil {
		t.Fatal("statement bound")
	}
}
func TestPlanAuthorityMetadata(t *testing.T) {
	p := testPlan()
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Plan){func(p *Plan) { p.Source.TLSCASecret.UID = "" }, func(p *Plan) { p.Source.ServiceUID = "" }, func(p *Plan) { p.Source.TLSServerName = "attacker.example" }, func(p *Plan) { p.Source.Credentials.Username = "ROOT" }, func(p *Plan) { p.DDLMode = "schema-before-after" }, func(p *Plan) { p.EnvironmentCreatedAt = time.Time{} }, func(p *Plan) { p.Target.Credentials.Ref.UID = "" }, func(p *Plan) { p.ConfigImage = "runner:latest" }} {
		bad := p
		mutate(&bad)
		if bad.Validate() == nil {
			t.Fatal("unbound metadata accepted")
		}
	}
}
func TestLifecycleDigestCanonicalUTC(t *testing.T) {
	p := testPlan()
	a, err := p.Digest()
	if err != nil {
		t.Fatal(err)
	}
	p.EnvironmentCreatedAt = p.EnvironmentCreatedAt.In(time.FixedZone("fixture-offset", 7200))
	b, err := p.Digest()
	if err != nil || a != b {
		t.Fatal("lifecycle timezone changed immutable receipt binding")
	}
}
func TestTLSServiceHostnameNotExternalPrefix(t *testing.T) {
	p := testPlan()
	for _, host := range []string{"mysql.source.svc", "mysql.source.svc.cluster.local"} {
		p.Source.TLSServerName = host
		if p.Validate() != nil {
			t.Fatal("internal service hostname refused")
		}
	}
	for _, host := range []string{"mysql.source.svc.attacker.example", "mysql.source.svc.cluster.local.attacker.example", "mysql.source.svc.custom-domain"} {
		p.Source.TLSServerName = host
		if p.Validate() == nil {
			t.Fatal("external prefix-suffix hostname accepted")
		}
	}
}
