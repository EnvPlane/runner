package pvccopy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"time"

	"github.com/envplane/contracts/domain"
)

// DomainExecutor supplies trusted scan/tenancy and maintenance authority. These
// fields must come from authenticated local/CP configuration, NOT the job body.
// Authorize must enforce Flux-only and an externally held owner maintenance
// fence through return. Production approval remains exact to source UID.
type DomainExecutor struct {
	Kubectl    *Kubectl
	Permission domain.PVCCopyPermission
	Sources    []domain.PVCCopySource
	Authorize  func(context.Context, domain.PVCCopyPlan, []domain.PVCCopySource) error
}

type domainPVC struct {
	pvcObject
	Spec struct {
		VolumeName    string   `json:"volumeName"`
		VolumeMode    string   `json:"volumeMode"`
		StorageClass  string   `json:"storageClassName"`
		AccessModes   []string `json:"accessModes"`
		DataSource    any      `json:"dataSource"`
		DataSourceRef any      `json:"dataSourceRef"`
		Resources     struct {
			Requests map[string]string `json:"requests"`
		} `json:"resources"`
	} `json:"spec"`
}

func (d DomainExecutor) readPVC(ctx context.Context, ref PVCRef, optional bool) (*domainPVC, error) {
	args := []string{"get", "pvc", ref.Name, "-n", ref.Namespace, "-o", "json", "--request-timeout=30s"}
	if optional {
		args = append(args, "--ignore-not-found=true")
	}
	b := &boundedBuffer{Limit: 1 << 20}
	if err := d.Kubectl.run(ctx, args, nil, b); err != nil {
		return nil, err
	}
	if optional && len(bytes.TrimSpace(b.Bytes())) == 0 {
		return nil, nil
	}
	var obj domainPVC
	if err := json.Unmarshal(b.Bytes(), &obj); err != nil {
		return nil, err
	}
	if obj.Metadata.Namespace != ref.Namespace || obj.Metadata.Name != ref.Name || obj.Metadata.UID == "" || obj.Metadata.DeletionTimestamp != nil {
		return nil, ErrUnsafe
	}
	return &obj, nil
}

var quantityPattern = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)([KMGTPE]i|[kMGTPE])?$`)

// Kubernetes quantities are parsed without a k8sGo dependency. The native
// binary/decimal byte subset is supported; uncommon forms fail closed.
func quantityBytes(s string) (int64, error) {
	m := quantityPattern.FindStringSubmatch(s)
	if m == nil {
		return 0, ErrUnsafe
	}
	n, ok := new(big.Rat).SetString(m[1])
	if !ok {
		return 0, ErrUnsafe
	}
	multipliers := map[string]int64{"": 1, "Ki": 1 << 10, "Mi": 1 << 20, "Gi": 1 << 30, "Ti": 1 << 40, "Pi": 1 << 50, "Ei": 1 << 60, "k": 1000, "M": 1000000, "G": 1000000000, "T": 1000000000000, "P": 1000000000000000, "E": 1000000000000000000}
	n.Mul(n, new(big.Rat).SetInt64(multipliers[m[2]]))
	q, r := new(big.Int), new(big.Int)
	q.QuoRem(n.Num(), n.Denom(), r)
	if r.Sign() != 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() || q.Sign() <= 0 {
		return 0, ErrUnsafe
	}
	return q.Int64(), nil
}
func sameSet(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

func (d DomainExecutor) validateSources(ctx context.Context, p domain.PVCCopyPlan) ([]domain.PVCCopySource, error) {
	fresh := make([]domain.PVCCopySource, 0, len(p.Items))
	for _, item := range p.Items {
		ref := PVCRef{item.SourceNamespace, item.SourceName, item.SourceUID}
		obj, err := d.readPVC(ctx, ref, false)
		if err != nil {
			return nil, err
		}
		if obj.Metadata.UID != ref.UID || obj.Metadata.Annotations["envplane.io/pvc-copy-offline"] != "true" || obj.Status.Phase != "Bound" || obj.Spec.VolumeName == "" || (obj.Spec.VolumeMode != "" && obj.Spec.VolumeMode != "Filesystem") {
			return nil, fmt.Errorf("%w: source identity or OFFLINE declaration", ErrUnsafe)
		}
		var trusted *domain.PVCCopySource
		for i := range d.Sources {
			s := &d.Sources[i]
			if s.Namespace == ref.Namespace && s.Name == ref.Name {
				if trusted != nil {
					return nil, ErrUnsafe
				}
				trusted = s
			}
		}
		if trusted == nil || trusted.UID != ref.UID {
			return nil, fmt.Errorf("%w: missing trusted source authority", ErrUnsafe)
		}
		liveClass := obj.Metadata.Labels["envplane.io/environment-class"]
		switch liveClass {
		case "development", "staging", "test", "preview", "production":
		default:
			liveClass = "production"
		}
		if liveClass != trusted.EnvironmentClass {
			return nil, fmt.Errorf("%w: fresh source classification drift", ErrUnsafe)
		}
		requested, err := quantityBytes(obj.Spec.Resources.Requests["storage"])
		if err != nil {
			return nil, err
		}
		// Tenant and environment class remain trusted scan metadata; labels are
		// never used to manufacture authority or production approval.
		s := *trusted
		s.StorageClass = obj.Spec.StorageClass
		s.AccessModes = slices.Clone(obj.Spec.AccessModes)
		s.RequestedBytes = requested
		s.VolumeMode = "Filesystem"
		identity, err := s.CanonicalIdentity()
		if err != nil {
			return nil, err
		}
		old, err := trusted.CanonicalIdentity()
		if err != nil || identity != old {
			return nil, fmt.Errorf("%w: fresh source spec drift", ErrUnsafe)
		}
		fresh = append(fresh, s)
		if err := d.Kubectl.CheckControllers(ctx, ref); err != nil {
			return nil, err
		}
	}
	if err := domain.ValidatePVCCopyPlan(p, fresh, d.Permission); err != nil {
		return nil, err
	}
	if err := d.Authorize(ctx, p, fresh); err != nil {
		return nil, err
	}
	return fresh, nil
}

func (d DomainExecutor) preflight(ctx context.Context, p domain.PVCCopyPlan) error {
	if _, err := d.validateSources(ctx, p); err != nil {
		return err
	}
	for _, item := range p.Items {
		pods, err := d.Kubectl.ListPods(ctx, item.SourceNamespace)
		if err != nil {
			return err
		}
		for _, pod := range pods {
			if pod.Namespace != item.SourceNamespace || pod.UID == "" {
				return ErrUnsafe
			}
			if pod.Phase == "Succeeded" || pod.Phase == "Failed" {
				continue
			}
			for _, claim := range pod.Claims {
				if claim == item.SourceName {
					return fmt.Errorf("%w: active source Pod", ErrUnsafe)
				}
			}
		}
	}
	return ctx.Err()
}

func labelIdentity(value string) string {
	h := sha256.Sum256([]byte(value))
	return hex.EncodeToString(h[:])[:40]
}
func targetLabels(p domain.PVCCopyPlan) map[string]string {
	return map[string]string{"envplane.io/pvc-copy-plan": labelIdentity(p.Digest), "envplane.io/pvc-copy-tenant": labelIdentity(p.TenantID), "envplane.io/pvc-copy-project": labelIdentity(p.ProjectID), "envplane.io/pvc-copy-environment": labelIdentity(p.EnvironmentID)}
}
func targetAnnotations(p domain.PVCCopyPlan, item domain.PVCCopyItem) map[string]string {
	return map[string]string{"envplane.io/pvc-copy-plan": p.Digest, "envplane.io/pvc-copy-item": item.ID, "envplane.io/pvc-copy-source-uid": item.SourceUID, "envplane.io/tenant": p.TenantID, "envplane.io/project": p.ProjectID, "envplane.io/environment": p.EnvironmentID}
}

func ownedTarget(obj *domainPVC, p domain.PVCCopyPlan, item domain.PVCCopyItem) error {
	if obj == nil || obj.Metadata.UID == "" || obj.Metadata.Namespace != p.TargetNamespace || obj.Metadata.Name != item.TargetName || obj.Metadata.DeletionTimestamp != nil {
		return ErrUnsafe
	}
	for key, value := range targetLabels(p) {
		if obj.Metadata.Labels[key] != value {
			return fmt.Errorf("%w: foreign target", ErrUnsafe)
		}
	}
	for key, value := range targetAnnotations(p, item) {
		if obj.Metadata.Annotations[key] != value {
			return fmt.Errorf("%w: foreign target", ErrUnsafe)
		}
	}
	n, err := quantityBytes(obj.Spec.Resources.Requests["storage"])
	if err != nil {
		return err
	}
	if obj.Spec.StorageClass != item.StorageClass || !sameSet(obj.Spec.AccessModes, item.AccessModes) || n != item.RequestedBytes || obj.Spec.VolumeMode != "Filesystem" || obj.Spec.DataSource != nil || obj.Spec.DataSourceRef != nil {
		return fmt.Errorf("%w: target spec drift or nonnative binding", ErrUnsafe)
	}
	return nil
}

// ExecuteDomain preflights ALL sources before any target write. It creates
// native PVCs only (no dataSource/sourcebinding), never adopts foreign claims,
// and retains owned targets on failure. WFFC binding is triggered by helpers.
// Completion is per item; no environment-ready claim is made here.
func (d DomainExecutor) ExecuteDomain(ctx context.Context, p domain.PVCCopyPlan, trustedSourceNamespaces []string, trustedImage string) ([]Marker, error) {
	if d.Kubectl == nil || d.Authorize == nil {
		return nil, fmt.Errorf("missing trusted domain executor authority")
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if p.Image != trustedImage || !imageRef.MatchString(trustedImage) || d.Kubectl.RunnerImage != trustedImage {
		return nil, ErrUnsafe
	}
	for _, ns := range p.AllowedSourceNamespaces {
		if !slices.Contains(trustedSourceNamespaces, ns) {
			return nil, fmt.Errorf("%w: trusted namespace denied", ErrUnsafe)
		}
	}
	if p.TimeoutSeconds > int64(MaxCopyTimeout/time.Second) || p.MaxBytes > MaxCopyBytes || len(p.Items) > 1000 {
		return nil, fmt.Errorf("%w: local domain bounds", ErrUnsafe)
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(p.TimeoutSeconds)*time.Second)
	defer cancel()
	if err := d.preflight(ctx, p); err != nil {
		return nil, err
	}
	// Probe admission/PodSecurity before any real target writes, especially when
	// the trusted root policy is required by source ownership or volume roots.
	for _, item := range p.Items {
		for i, ref := range []PVCRef{{item.SourceNamespace, item.SourceName, item.SourceUID}, {Namespace: p.TargetNamespace, Name: item.TargetName}} {
			name := "pvccopy-target-preflight"
			if i == 0 {
				name = SourceHelperName(ref)
			}
			s := HelperSpec{Namespace: ref.Namespace, Name: name, Owner: p.Digest, PVC: ref, ReadOnly: i == 0, Image: p.Image, Timeout: time.Duration(item.TimeoutSeconds) * time.Second}
			if err := d.Kubectl.PreflightHelper(ctx, s); err != nil {
				return nil, err
			}
		}
	}
	var results []Marker
	for _, item := range p.Items {
		// Repeat source refusal immediately before creating or opening a target.
		if err := d.preflight(ctx, p); err != nil {
			return results, err
		}
		ref := PVCRef{Namespace: p.TargetNamespace, Name: item.TargetName}
		pods, err := d.Kubectl.ListPods(ctx, p.TargetNamespace)
		if err != nil {
			return results, err
		}
		for _, pod := range pods {
			if pod.Namespace != p.TargetNamespace || pod.UID == "" {
				return results, ErrUnsafe
			}
			if pod.Phase == "Succeeded" || pod.Phase == "Failed" {
				continue
			}
			for _, claim := range pod.Claims {
				if claim == ref.Name {
					return results, fmt.Errorf("%w: target already consumed", ErrUnsafe)
				}
			}
		}
		obj, err := d.readPVC(ctx, ref, true)
		if err != nil {
			return results, err
		}
		if obj == nil {
			labels := targetLabels(p)
			labels["envplane.io/managed"] = "true"
			labels["envplane.io/project"] = p.ProjectID
			labels["envplane.io/environment"] = p.EnvironmentID
			labels["envplane.io/project-id"] = p.ProjectID
			labels["envplane.io/environment-id"] = p.EnvironmentID
			manifest := map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]any{"namespace": p.TargetNamespace, "name": item.TargetName, "labels": labels, "annotations": targetAnnotations(p, item)}, "spec": map[string]any{"storageClassName": item.StorageClass, "volumeMode": "Filesystem", "accessModes": item.AccessModes, "resources": map[string]any{"requests": map[string]string{"storage": fmt.Sprint(item.RequestedBytes)}}}}
			var created domainPVC
			if err := d.Kubectl.json(ctx, []string{"create", "-n", p.TargetNamespace, "-f", "-", "-o", "json", "--request-timeout=30s"}, manifest, &created); err != nil {
				return results, err
			}
			obj = &created
		}
		if err := ownedTarget(obj, p, item); err != nil {
			return results, err
		}
		ref.UID = obj.Metadata.UID
		leaf := Plan{ImmutablePlanDigest: p.Digest, ItemID: item.ID, Tenant: p.TenantID, Project: p.ProjectID, Environment: p.EnvironmentID, Source: PVCRef{item.SourceNamespace, item.SourceName, item.SourceUID}, Target: ref, MaxBytes: item.MaxBytes, MaxEntries: MaxCopyEntries, Timeout: time.Duration(item.TimeoutSeconds) * time.Second, Image: p.Image, AllowedSourceNamespaces: slices.Clone(p.AllowedSourceNamespaces), SourceQuiescedOffline: true}
		leaf.AllowedTarget = TargetBinding{leaf.Tenant, leaf.Project, leaf.Environment, ref}
		executor := Executor{Transport: d.Kubectl, Policy: Policy{RunnerImage: trustedImage, SourceNamespaces: slices.Clone(trustedSourceNamespaces), Target: leaf.AllowedTarget}}
		executor.Permission = func(ctx context.Context, _ Plan, _ PVC, _ PVC) error {
			if _, err := d.validateSources(ctx, p); err != nil {
				return err
			}
			target, err := d.readPVC(ctx, ref, false)
			if err != nil {
				return err
			}
			if target.Metadata.UID != ref.UID {
				return ErrUnsafe
			}
			return ownedTarget(target, p, item)
		}
		marker, err := executor.Execute(ctx, leaf)
		if err != nil {
			return results, err
		}
		results = append(results, marker)
	}
	return results, nil
}
