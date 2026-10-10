package pvccopy

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

type PVC struct {
	Ref             PVCRef
	VolumeName      string
	Filesystem      bool
	Bound           bool
	Deleting        bool
	OfflineDeclared bool
	Pending         bool
}

type Pod struct {
	Namespace string
	Name      string
	UID       string
	Phase     string
	Claims    []string
}

// Helper identity is taken from the create response, never reconstructed from
// a label selector. DeleteHelper MUST enforce UID in API DeleteOptions.
type Helper struct {
	Namespace string
	Name      string
	UID       string
	Owner     string
	PVC       PVCRef
	ReadOnly  bool
	Image     string
}

type HelperSpec struct {
	Namespace string
	Name      string
	Owner     string
	PVC       PVCRef
	ReadOnly  bool
	Image     string
	Timeout   time.Duration
}

// SourceHelperName supports exact resourceNames permissions for pods/exec and
// helper deletion. Existing same-name Pods are NEVER adopted or deleted on a
// create conflict; the owner must clean up an orphan using its known identity.
func SourceHelperName(ref PVCRef) string {
	h := sha256.Sum256([]byte(ref.Namespace + "/" + ref.Name + "/" + ref.UID))
	return "pvccopy-source-" + hex.EncodeToString(h[:12])
}

// Transport operations must honor context and return only after I/O goroutines
// are joined. No method may modify a PVC, workload, service account, or RBAC.
type Transport interface {
	GetPVC(context.Context, PVCRef) (PVC, error)
	ListPods(context.Context, string) ([]Pod, error)
	CheckControllers(context.Context, PVCRef) error
	CreateHelper(context.Context, HelperSpec) (Helper, error)
	WaitReady(context.Context, Helper) error
	Exec(context.Context, Helper, []string, io.Reader, io.Writer) error
	DeleteHelper(context.Context, Helper) error
}

// Policy is trusted local configuration, never decoded from a job/Plan. A
// contract adapter must resolve SourceUID and the newly created target UID.
type Policy struct {
	RunnerImage      string
	SourceNamespaces []string
	Target           TargetBinding
}

func (policy Policy) check(p Plan) error {
	if !imageRef.MatchString(policy.RunnerImage) || p.Image != policy.RunnerImage || policy.Target != (TargetBinding{p.Tenant, p.Project, p.Environment, p.Target}) {
		return fmt.Errorf("%w: trusted image or target policy denied", ErrUnsafe)
	}
	for _, ns := range policy.SourceNamespaces {
		if ns == p.Source.Namespace {
			return nil
		}
	}
	return fmt.Errorf("%w: trusted source namespace policy denied", ErrUnsafe)
}

// Permission is mandatory. It must revalidate trusted source specification,
// tenant/project ownership, OFFLINE fencing, quota/backend eligibility (Flux
// only initially), and target provisioning authority, not trust job assertions.
// PVC arguments are freshly read each time; no Kubernetes mutations are needed.
type Permission func(context.Context, Plan, PVC, PVC) error

type Executor struct {
	Transport  Transport
	Policy     Policy
	Permission Permission
}

func (e Executor) check(ctx context.Context, p Plan, helpers []Helper) error {
	if err := e.Policy.check(p); err != nil {
		return err
	}
	source, err := e.Transport.GetPVC(ctx, p.Source)
	if err != nil {
		return err
	}
	target, err := e.Transport.GetPVC(ctx, p.Target)
	if err != nil {
		return err
	}
	for i, pair := range []struct {
		got  PVC
		want PVCRef
	}{{source, p.Source}, {target, p.Target}} {
		bindingOK := pair.got.Bound && pair.got.VolumeName != ""
		if i == 1 && len(helpers) < 2 && pair.got.Pending {
			bindingOK = true
		}
		if pair.got.Ref != pair.want || !pair.got.Filesystem || !bindingOK || pair.got.Deleting {
			return fmt.Errorf("%w: PVC identity, phase, or mode", ErrUnsafe)
		}
	}
	if target.VolumeName != "" && source.VolumeName == target.VolumeName {
		return fmt.Errorf("%w: same backing PV", ErrUnsafe)
	}
	if !source.OfflineDeclared {
		return fmt.Errorf("%w: source lacks owner OFFLINE declaration", ErrUnsafe)
	}
	if err := e.Transport.CheckControllers(ctx, p.Source); err != nil {
		return err
	}
	if err := e.Permission(ctx, p, source, target); err != nil {
		return fmt.Errorf("PVC copy permission: %w", err)
	}
	namespaces := []string{p.Source.Namespace}
	if p.Target.Namespace != p.Source.Namespace {
		namespaces = append(namespaces, p.Target.Namespace)
	}
	for _, ns := range namespaces {
		pods, err := e.Transport.ListPods(ctx, ns)
		if err != nil {
			return err
		}
		for _, pod := range pods {
			if pod.Namespace != ns || pod.UID == "" {
				return fmt.Errorf("%w: unknown pod identity", ErrUnsafe)
			}
			owned := false
			for _, h := range helpers {
				if h.UID == pod.UID && h.Name == pod.Name && h.Namespace == pod.Namespace {
					owned = true
				}
			}
			if owned {
				continue
			}
			// Pending, Unknown, and terminating nonterminal Pods are consumers too.
			if pod.Phase == "Succeeded" || pod.Phase == "Failed" {
				continue
			}
			for _, claim := range pod.Claims {
				if (ns == p.Source.Namespace && claim == p.Source.Name) || (ns == p.Target.Namespace && claim == p.Target.Name) {
					return fmt.Errorf("%w: PVC has an active consumer", ErrUnsafe)
				}
			}
		}
	}
	return ctx.Err()
}

func helperArgs(command string, p Plan) []string {
	b, _ := json.Marshal(p)
	return []string{HelperBinary, command, string(b)}
}

func (e Executor) rpc(ctx context.Context, h Helper, command string, p Plan, extra any, out any) error {
	args := helperArgs(command, p)
	if extra != nil {
		b, err := json.Marshal(extra)
		if err != nil {
			return err
		}
		args = append(args, string(b))
	}
	buf := &boundedBuffer{Limit: 16384}
	if err := e.Transport.Exec(ctx, h, args, nil, buf); err != nil {
		return err
	}
	return decodeJSON(buf.Bytes(), out)
}

// Execute creates only two helper Pods. The target PVC must already be newly
// provisioned and bound; its actual UID must be included in the authorized plan.
// There is no automatic retry or partial cleanup, and no environment-ready event.
func (e Executor) Execute(ctx context.Context, p Plan) (result Marker, retErr error) {
	if err := p.Validate(); err != nil {
		return Marker{}, err
	}
	if e.Transport == nil || e.Permission == nil {
		return Marker{}, errors.New("missing PVC copy transport or trusted permission")
	}
	if err := e.Policy.check(p); err != nil {
		return Marker{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, p.Timeout)
	defer cancel()
	if err := e.check(ctx, p, nil); err != nil {
		return Marker{}, err
	}
	digest, _ := p.Digest()
	var entropy [12]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return Marker{}, err
	}
	owner := digest + "-" + hex.EncodeToString(entropy[:])
	var helpers []Helper
	defer func() {
		// A separate, hard-bounded cleanup budget also works after cancellation.
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		for i := len(helpers) - 1; i >= 0; i-- {
			if err := e.Transport.DeleteHelper(cleanup, helpers[i]); err != nil {
				retErr = errors.Join(retErr, fmt.Errorf("helper cleanup: %w", err))
			}
		}
	}()
	for i, ref := range []PVCRef{p.Source, p.Target} {
		name := "pvccopy-target-" + hex.EncodeToString(entropy[:])
		if i == 0 {
			name = SourceHelperName(ref)
		}
		spec := HelperSpec{ref.Namespace, name, owner, ref, i == 0, p.Image, p.Timeout}
		h, err := e.Transport.CreateHelper(ctx, spec)
		if err != nil {
			// Only a known exact create identity is eligible for bounded cleanup.
			if h.UID != "" && h.Namespace == spec.Namespace && h.Name == spec.Name && h.Owner == owner {
				helpers = append(helpers, h)
			}
			return Marker{}, err
		}
		if h.UID == "" || h.Namespace != spec.Namespace || h.Name != spec.Name || h.Owner != owner || h.PVC != spec.PVC || h.ReadOnly != spec.ReadOnly || h.Image != spec.Image {
			return Marker{}, fmt.Errorf("%w: unknown helper creation outcome", ErrUnsafe)
		}
		helpers = append(helpers, h)
		if err := e.Transport.WaitReady(ctx, h); err != nil {
			return Marker{}, err
		}
		if err := e.check(ctx, p, helpers); err != nil {
			return Marker{}, err
		}
	}
	if err := e.check(ctx, p, helpers); err != nil {
		return Marker{}, err
	}
	var existing *Marker
	if err := e.rpc(ctx, helpers[1], "pvc-verify", p, nil, &existing); err != nil {
		return Marker{}, err
	}
	if existing != nil {
		if *existing != markerFor(p, existing.Receipt) {
			return Marker{}, ErrPartial
		}
		if err := e.check(ctx, p, helpers); err != nil {
			return Marker{}, err
		}
		return *existing, nil
	}
	if err := e.check(ctx, p, helpers); err != nil {
		return Marker{}, err
	}
	streamCtx, stopStream := context.WithCancel(ctx)
	defer stopStream()
	sr, sw := io.Pipe()
	tr, tw := io.Pipe()
	defer sr.Close()
	defer sw.Close()
	defer tr.Close()
	defer tw.Close()
	stop := context.AfterFunc(streamCtx, func() {
		sr.CloseWithError(streamCtx.Err())
		sw.CloseWithError(streamCtx.Err())
		tr.CloseWithError(streamCtx.Err())
		tw.CloseWithError(streamCtx.Err())
	})
	defer stop()
	sourceDone := make(chan error, 1)
	targetDone := make(chan error, 1)
	response := &boundedBuffer{Limit: 16384}
	go func() {
		err := e.Transport.Exec(streamCtx, helpers[0], helperArgs("pvc-export", p), nil, sw)
		sw.CloseWithError(err)
		if err != nil {
			stopStream()
		}
		sourceDone <- err
	}()
	go func() {
		err := e.Transport.Exec(streamCtx, helpers[1], helperArgs("pvc-import", p), tr, response)
		tr.CloseWithError(err)
		if err != nil {
			stopStream()
		}
		targetDone <- err
	}()
	receipt, relayErr := Relay(streamCtx, p, sr, tw)
	sr.CloseWithError(relayErr)
	tw.CloseWithError(relayErr)
	if relayErr != nil {
		stopStream()
	}
	sourceErr, targetErr := <-sourceDone, <-targetDone
	if err := errors.Join(relayErr, sourceErr, targetErr); err != nil {
		return Marker{}, err
	}
	var received Receipt
	if err := decodeJSON(response.Bytes(), &received); err != nil {
		return Marker{}, err
	}
	if received != receipt {
		return Marker{}, fmt.Errorf("%w: stream receipt mismatch", ErrUnsafe)
	}
	// Do not publish completion if the PVC identities or offline checks drifted.
	if err := e.check(ctx, p, helpers); err != nil {
		return Marker{}, err
	}
	// Re-read the source after transfer. Matching transport receipts alone prove
	// delivery, not that an externally violated offline fence left source stable.
	verifyReader, verifyWriter := io.Pipe()
	verifyDone := make(chan error, 1)
	verifyStop := context.AfterFunc(ctx, func() { verifyReader.CloseWithError(ctx.Err()); verifyWriter.CloseWithError(ctx.Err()) })
	go func() {
		err := e.Transport.Exec(ctx, helpers[0], helperArgs("pvc-export", p), nil, verifyWriter)
		verifyWriter.CloseWithError(err)
		verifyDone <- err
	}()
	verified, verifyErr := Relay(ctx, p, verifyReader, io.Discard)
	verifyReader.CloseWithError(verifyErr)
	verifySourceErr := <-verifyDone
	verifyStop()
	verifyWriter.Close()
	if err := errors.Join(verifyErr, verifySourceErr); err != nil {
		return Marker{}, err
	}
	if verified != receipt {
		return Marker{}, fmt.Errorf("%w: source changed during transfer", ErrUnsafe)
	}
	if err := e.check(ctx, p, helpers); err != nil {
		return Marker{}, err
	}
	var marker Marker
	if err := e.rpc(ctx, helpers[1], "pvc-complete", p, receipt, &marker); err != nil {
		return Marker{}, err
	}
	if marker != markerFor(p, receipt) {
		return Marker{}, ErrPartial
	}
	if err := e.check(ctx, p, helpers); err != nil {
		return Marker{}, err
	}
	return marker, nil
}
