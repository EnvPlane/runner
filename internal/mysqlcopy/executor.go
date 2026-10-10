package mysqlcopy

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"io"
	"slices"
	"time"
)

// CredentialCheck holds only hashes of exact approved Secret values in RAM,
// never passwords in plan/receipt/logs. Transport MUST NOT expose raw failures.
type CredentialCheck struct {
	Source, Target, TargetRoot, Backup, CA [32]byte
	Principals                             [3][32]byte
}
type Hold interface {
	Check(context.Context) error
	Release(context.Context) error
}
type Transport interface {
	CheckCredentials(context.Context, Plan) (CredentialCheck, error)
	Source(context.Context, Plan) (SourceIdentity, error)
	HoldDDL(context.Context, Plan) (Hold, error)
	Inspect(context.Context, Plan) (*Receipt, error)
	Provision(context.Context, Plan) (TargetIdentity, error)
	Dump(context.Context, Plan, io.Writer) error
	Restore(context.Context, Plan, TargetIdentity, io.Reader) error
	TargetDump(context.Context, Plan, TargetIdentity, io.Writer) error
	Shutdown(context.Context, Plan, TargetIdentity) error
	Commit(context.Context, Plan, Receipt) error
	Cleanup(context.Context, Plan) error
}

type DomainExecutor struct {
	Transport Transport
	Authority Authority
}
type Executor = DomainExecutor

func (e DomainExecutor) gate(ctx context.Context, p Plan, s Stage) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if e.Authority(ctx, p, s) != nil {
		return ErrUnsafe
	}
	return nil
}
func safeError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return ErrOperation
}

// ExecuteDomain consumes a metadata-only plan; main/Hume own domain adaptation,
// release-job serialization, source quiescence proof and publication gates.
func (e DomainExecutor) ExecuteDomain(ctx context.Context, p Plan) (result Receipt, retErr error) {
	if p.Validate() != nil || e.Transport == nil || e.Authority == nil {
		return Receipt{}, ErrUnsafe
	}
	defer func() {
		if retErr != nil {
			result = Receipt{}
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	defer func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer stop()
		if e.Transport.Cleanup(cleanup, p) != nil {
			retErr = ErrOperation
		}
	}()
	if err := e.gate(ctx, p, ReadSource); err != nil {
		return Receipt{}, err
	}
	credentials, err := e.Transport.CheckCredentials(ctx, p)
	if err != nil {
		return Receipt{}, safeError(ctx, err)
	}
	if subtle.ConstantTimeCompare(credentials.Source[:], credentials.Target[:]) == 1 {
		return Receipt{}, ErrUnsafe
	}
	if credentials.Source == ([32]byte{}) || credentials.Target == ([32]byte{}) || credentials.TargetRoot == ([32]byte{}) {
		return Receipt{}, ErrUnsafe
	}
	if subtle.ConstantTimeCompare(credentials.Target[:], credentials.TargetRoot[:]) == 1 || subtle.ConstantTimeCompare(credentials.Source[:], credentials.TargetRoot[:]) == 1 {
		return Receipt{}, ErrUnsafe
	}
	source, err := e.Transport.Source(ctx, p)
	if err != nil {
		return Receipt{}, safeError(ctx, err)
	}
	if err := validateSource(p, source); err != nil {
		return Receipt{}, err
	}
	if err := e.gate(ctx, p, HoldDDL); err != nil {
		return Receipt{}, err
	}
	hold, err := e.Transport.HoldDDL(ctx, p)
	if err != nil || hold == nil {
		return Receipt{}, ErrUnsafe
	}
	defer func() {
		release, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		if hold.Release(release) != nil {
			retErr = ErrOperation
		}
	}()
	check := func(stage Stage) error {
		if err := e.gate(ctx, p, stage); err != nil {
			return err
		}
		if hold.Check(ctx) != nil {
			return ErrUnsafe
		}
		fresh, err := e.Transport.CheckCredentials(ctx, p)
		if err != nil {
			return safeError(ctx, err)
		}
		if fresh != credentials {
			return ErrUnsafe
		}
		freshSource, err := e.Transport.Source(ctx, p)
		if err != nil {
			return safeError(ctx, err)
		}
		if validateSource(p, freshSource) != nil || !sameIdentity(source, freshSource) {
			return ErrUnsafe
		}
		return nil
	}
	if err := check(ProvisionTarget); err != nil {
		return Receipt{}, err
	}
	existing, err := e.Transport.Inspect(ctx, p)
	if err != nil {
		return Receipt{}, ErrPartial
	}
	digest, _ := p.Digest()
	if existing != nil {
		if existing.Version != 1 || existing.PlanDigest != digest || existing.Binding != p.Binding || existing.SourcePVCUID != p.Source.PVC.UID || existing.SourceStatefulSetUID != p.Source.StatefulSet.UID || existing.SourceServerUUID != source.ServerUUID || !existing.ShutdownVerified {
			return Receipt{}, ErrPartial
		}
		// Transport re-verifies persisted target data before returning a receipt.
		if err := check(Complete); err != nil {
			return Receipt{}, err
		}
		return *existing, nil
	}
	target, err := e.Transport.Provision(ctx, p)
	if err != nil {
		return Receipt{}, safeError(ctx, err)
	}
	if target.PVCUID == "" || target.PodUID == "" || target.SecretUID != p.Target.Credentials.Ref.UID || target.PVCUID == p.Source.PVC.UID {
		return Receipt{}, ErrUnsafe
	}
	if err := check(Transfer); err != nil {
		return Receipt{}, err
	}
	reader, writer := io.Pipe()
	streamCtx, stopStream := context.WithCancel(ctx)
	defer stopStream()
	stop := context.AfterFunc(streamCtx, func() { _ = reader.CloseWithError(streamCtx.Err()); _ = writer.CloseWithError(streamCtx.Err()) })
	defer stop()
	dumpDone := make(chan error, 1)
	go func() { err := e.Transport.Dump(streamCtx, p, writer); _ = writer.CloseWithError(err); dumpDone <- err }()
	tracker := newTracker(p)
	validated := validateReader(reader, tracker)
	err = e.Transport.Restore(streamCtx, p, target, validated)
	if err == nil && !validated.exhausted() {
		err = ErrUnsafe
	}
	_ = reader.CloseWithError(err)
	if err != nil {
		stopStream()
	}
	dumpErr := <-dumpDone
	// Pipe cleanup only; producer/restore failures are checked separately.
	_ = writer.Close()
	_ = reader.Close()
	if err != nil || dumpErr != nil {
		return Receipt{}, safeError(ctx, ErrOperation)
	}
	proof, err := tracker.Finish()
	if err != nil {
		return Receipt{}, ErrUnsafe
	}
	if len(proof.Tables) != len(source.Tables) {
		return Receipt{}, ErrUnsafe
	}
	for _, table := range source.Tables {
		if !slices.ContainsFunc(proof.Tables, func(v TableProof) bool { return v.Name == table.Name }) {
			return Receipt{}, ErrUnsafe
		}
	}
	if err := check(Verify); err != nil {
		return Receipt{}, err
	}
	restored := newTracker(p)
	if err := e.Transport.TargetDump(ctx, p, target, restored); err != nil {
		return Receipt{}, safeError(ctx, err)
	}
	actual, err := restored.Finish()
	if err != nil || !sameProof(proof, actual) {
		return Receipt{}, ErrUnsafe
	}
	if err := check(Complete); err != nil {
		return Receipt{}, err
	}
	if err := e.Transport.Shutdown(ctx, p, target); err != nil {
		return Receipt{}, safeError(ctx, err)
	}
	if err := check(Complete); err != nil {
		return Receipt{}, err
	}
	result = Receipt{1, digest, p.Binding, p.Source.PVC.UID, p.Source.StatefulSet.UID, source.ServerUUID, target, proof, true}
	if err := e.Transport.Commit(ctx, p, result); err != nil {
		return Receipt{}, safeError(ctx, err)
	}
	return result, nil
}

func validateSource(p Plan, s SourceIdentity) error {
	if s.PodUID == "" || s.ServerUUID == "" || len(s.ServerUUID) > 128 || len(s.Version) < 2 || s.Version[:2] != "8." || len(s.Tables) == 0 || len(s.Tables) > p.MaxTables {
		return ErrUnsafe
	}
	seen := map[string]bool{}
	for _, t := range s.Tables {
		if !identifier.MatchString(t.Name) || seen[t.Name] || !t.PrimaryKey {
			return ErrUnsafe
		}
		seen[t.Name] = true
		if t.Engine != "InnoDB" && (p.DDLMode != "quiescence" || (t.Engine != "MyISAM" && t.Engine != "MEMORY")) {
			return ErrUnsafe
		}
	}
	return nil
}
func sameIdentity(a, b SourceIdentity) bool {
	return a.PodUID == b.PodUID && a.ServerUUID == b.ServerUUID && a.Version == b.Version && slices.Equal(a.Tables, b.Tables)
}
func sameProof(a, b Proof) bool {
	// Raw dump bytes/hash remain source transfer audit, not logical equality:
	// concurrent DML can advance CREATE TABLE's AUTO_INCREMENT table option.
	return slices.Equal(a.Tables, b.Tables)
}
func valueHash(b []byte) [32]byte { return sha256.Sum256(b) }
func hashString(b []byte) string  { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
