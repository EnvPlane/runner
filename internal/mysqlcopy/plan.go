// Package mysqlcopy performs bounded logical MySQL copies. It never copies a
// database datadir and never publishes workloads or grants source privileges.
package mysqlcopy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"
)

// TargetDataSubPath is a publication invariant. The parent must seal this exact
// subPath into the generated target MySQL workload BEFORE materialization.
const TargetDataSubPath = "mysql"

var (
	ErrUnsafe      = errors.New("mysql copy safety refusal")
	ErrPartial     = errors.New("mysql copy partial or unknown target")
	ErrOperation   = errors.New("mysql copy operation failed (details redacted)")
	identifier     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{0,63}$`)
	name           = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)
	image          = regexp.MustCompile(`^[a-z0-9][a-z0-9./:_-]*@sha256:[0-9a-f]{64}$`)
	secretKey      = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,252}$`)
	contractDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	uidPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,127}$`)
)

type Ref struct{ Namespace, Name, UID string }
type CredentialRef struct {
	Ref         Ref
	Username    string
	UsernameKey string
	PasswordKey string
}
type Source struct {
	PVC                                             Ref
	StatefulSet                                     Ref
	Container, Image, Service, ServiceUID, Database string
	Port                                            int
	Credentials                                     CredentialRef
	BackupAdminSecretRef                            CredentialRef
	TLSCASecret                                     Ref
	TLSCAKey                                        string
	TLSServerName                                   string
}
type Target struct {
	Namespace, PVCName, StorageClass, Image, Database string
	RootPasswordKey                                   string
	RequestedBytes                                    int64
	Credentials                                       CredentialRef
}
type Plan struct {
	Binding, Tenant, Project, Environment             string
	EnvironmentCreatedAt                              time.Time
	DomainPlanDigest, SecretMaterializationPlanDigest string
	Source                                            Source
	Target                                            Target
	ConfigImage                                       string
	DDLMode                                           string // backup_lock or quiescence; NEVER inferred from a job flag.
	QuiescenceRef                                     string
	MaxBytes                                          int64
	MaxStatementBytes, MaxTables                      int
	MaxRows                                           int64
	Timeout                                           time.Duration
}

func (p Plan) Validate() error {
	for _, v := range []string{p.Binding, p.Tenant, p.Project, p.Environment} {
		if v == "" || len(v) > 256 {
			return ErrUnsafe
		}
	}
	for _, r := range []Ref{p.Source.PVC, p.Source.StatefulSet, p.Source.Credentials.Ref, p.Target.Credentials.Ref} {
		if !validRef(r) {
			return ErrUnsafe
		}
	}
	if p.Source.PVC.Namespace != p.Source.StatefulSet.Namespace || p.Source.PVC.Namespace != p.Source.Credentials.Ref.Namespace || p.Source.PVC.Namespace == p.Target.Namespace || p.Target.Credentials.Ref.Namespace != p.Target.Namespace {
		return ErrUnsafe
	}
	for _, v := range []string{p.Target.Namespace, p.Target.PVCName, p.Target.StorageClass, p.Source.Container, p.Source.Service} {
		if !name.MatchString(v) || len(v) > 253 {
			return ErrUnsafe
		}
	}
	if !uidPattern.MatchString(p.Source.ServiceUID) || p.Source.Port < 1 || p.Source.Port > 65535 || !identifier.MatchString(p.Source.Database) || p.Target.Database != p.Source.Database {
		return ErrUnsafe
	}
	switch strings.ToLower(p.Source.Database) {
	case "mysql", "sys", "information_schema", "performance_schema":
		return ErrUnsafe
	}
	if !image.MatchString(p.Source.Image) || p.Target.Image != p.Source.Image || !image.MatchString(p.ConfigImage) {
		return ErrUnsafe
	}
	if !validCredentials(p.Source.Credentials) || !validCredentials(p.Target.Credentials) {
		return ErrUnsafe
	}
	if !secretKey.MatchString(p.Target.RootPasswordKey) || p.Target.RootPasswordKey == p.Target.Credentials.PasswordKey {
		return ErrUnsafe
	}
	if !validRef(p.Source.TLSCASecret) || p.Source.TLSCASecret.Namespace != p.Source.PVC.Namespace || !secretKey.MatchString(p.Source.TLSCAKey) {
		return ErrUnsafe
	}
	dns := p.Source.Service + "." + p.Source.PVC.Namespace + ".svc"
	if p.Source.TLSServerName != dns && (!strings.HasPrefix(p.Source.TLSServerName, dns+".") || !name.MatchString(p.Source.TLSServerName)) {
		return ErrUnsafe
	}
	if strings.EqualFold(p.Source.Credentials.Username, "root") || strings.EqualFold(p.Target.Credentials.Username, "root") {
		return ErrUnsafe
	}
	if p.EnvironmentCreatedAt.IsZero() || !contractDigest.MatchString(p.DomainPlanDigest) || !contractDigest.MatchString(p.SecretMaterializationPlanDigest) {
		return ErrUnsafe
	}
	switch p.DDLMode {
	case "backup_lock":
		if !validRef(p.Source.BackupAdminSecretRef.Ref) || !validCredentials(p.Source.BackupAdminSecretRef) || p.Source.BackupAdminSecretRef.Ref.Namespace != p.Source.PVC.Namespace || p.Source.BackupAdminSecretRef.Ref == p.Source.Credentials.Ref {
			return ErrUnsafe
		}
	case "quiescence":
		if p.QuiescenceRef == "" || len(p.QuiescenceRef) > 256 {
			return ErrUnsafe
		}
	default:
		return ErrUnsafe
	}
	if p.MaxBytes <= 0 || p.MaxBytes > 1<<40 || p.MaxStatementBytes < 1024 || p.MaxStatementBytes > 16<<20 || p.MaxTables < 1 || p.MaxTables > 1000 || p.MaxRows < 1 || p.MaxRows > 100000000 || p.Timeout <= 0 || p.Timeout > 6*time.Hour || p.Target.RequestedBytes < p.MaxBytes || p.Target.RequestedBytes > 1<<40 {
		return ErrUnsafe
	}
	return nil
}
func validRef(r Ref) bool {
	return uidPattern.MatchString(r.UID) && name.MatchString(r.Namespace) && len(r.Namespace) <= 63 && name.MatchString(r.Name) && len(r.Name) <= 253
}
func validCredentials(r CredentialRef) bool {
	return secretKey.MatchString(r.PasswordKey) && ((identifier.MatchString(r.Username) && r.UsernameKey == "") || (r.Username == "" && secretKey.MatchString(r.UsernameKey)))
}
func (p Plan) Digest() (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

type Stage string

const (
	ReadSource      Stage = "read_source"
	HoldDDL         Stage = "hold_ddl"
	ProvisionTarget Stage = "provision_target"
	Transfer        Stage = "transfer"
	Verify          Stage = "verify"
	Complete        Stage = "complete"
)

type Authority func(context.Context, Plan, Stage) error

type Table struct {
	Name, Engine string
	PrimaryKey   bool
}
type SourceIdentity struct {
	PodUID, ServerUUID, Version string
	Tables                      []Table
}
type TargetIdentity struct{ PVCUID, PodUID, SecretUID string }
type TableProof struct {
	Name                     string
	Rows                     int64
	SchemaSHA256, RowsSHA256 string
}
type Proof struct {
	Bytes  int64
	SHA256 string
	Tables []TableProof
}
type Receipt struct {
	Version                                                                   int
	PlanDigest, Binding, SourcePVCUID, SourceStatefulSetUID, SourceServerUUID string
	Target                                                                    TargetIdentity
	Proof                                                                     Proof
	ShutdownVerified                                                          bool
}
