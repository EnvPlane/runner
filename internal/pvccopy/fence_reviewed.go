package pvccopy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/envplane/contracts/domain"
)

// ReviewedAdmissionFenceRef must come from trusted deployment configuration
// populated by reviewed installation, never a command/job or browser payload.
// Whole-spec hashes bind the combined SQL/FS helper allowlist; UIDs refuse policy
// replacement even with the same name. No executor creates/deletes policies.
type ReviewedAdmissionFenceRef struct {
	Namespace                                                                           string
	PolicyName, PolicyUID, PolicySpecSHA256, BindingName, BindingUID, BindingSpecSHA256 string
}

// RequireReviewedAdmissionFence is the combined-policy counterpart to the
// canonical FS-only verifier. Exact raw-hex spec hashes pin the server-reviewed
// compiler result, with source-claim guards checked against THIS execution plan.
func (k *Kubectl) RequireReviewedAdmissionFence(ctx context.Context, p domain.PVCCopyPlan, runnerNS, runnerSA string, refs []ReviewedAdmissionFenceRef) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	_, _, authorityErr := fenceAuthority(p, runnerNS, runnerSA)
	if k == nil || p.Validate() != nil || authorityErr != nil {
		return fmt.Errorf("%w: reviewed source fence context invalid", ErrUnsafe)
	}
	checked := map[string]string{}
	for _, item := range p.Items {
		var ref *ReviewedAdmissionFenceRef
		for i := range refs {
			if refs[i].Namespace == item.SourceNamespace {
				if ref != nil {
					return fmt.Errorf("%w: ambiguous reviewed source fence", ErrUnsafe)
				}
				ref = &refs[i]
			}
		}
		if ref == nil || ref.PolicyUID == "" || ref.BindingUID == "" || ref.PolicyName != ref.BindingName || !strings.HasPrefix(ref.PolicyName, "envplane-pvc-copy-fence-") || len(ref.PolicySpecSHA256) != 64 || len(ref.BindingSpecSHA256) != 64 {
			return fmt.Errorf("%w: reviewed source fence receipt missing", ErrUnsafe)
		}
		var policy, binding fenceObject
		if err := k.json(ctx, []string{"get", "validatingadmissionpolicies.admissionregistration.k8s.io", ref.PolicyName, "-o", "json", "--request-timeout=30s"}, nil, &policy); err != nil {
			return fmt.Errorf("%w: reviewed source fence policy unavailable", ErrUnsafe)
		}
		if err := k.json(ctx, []string{"get", "validatingadmissionpolicybindings.admissionregistration.k8s.io", ref.BindingName, "-o", "json", "--request-timeout=30s"}, nil, &binding); err != nil {
			return fmt.Errorf("%w: reviewed source fence binding unavailable", ErrUnsafe)
		}
		hash := func(raw json.RawMessage) string {
			var spec any
			if json.Unmarshal(raw, &spec) != nil {
				return ""
			}
			b, _ := json.Marshal(spec)
			h := sha256.Sum256(b)
			return hex.EncodeToString(h[:])
		}
		d := policy.Metadata.Annotations[fenceDigestAnnotation]
		if policy.Metadata.Name != ref.PolicyName || binding.Metadata.Name != ref.BindingName || policy.Metadata.UID != ref.PolicyUID || binding.Metadata.UID != ref.BindingUID || policy.Metadata.DeletionTimestamp != nil || binding.Metadata.DeletionTimestamp != nil || hash(policy.Spec) != ref.PolicySpecSHA256 || hash(binding.Spec) != ref.BindingSpecSHA256 || !fenceIdentityDigest.MatchString(d) || ref.PolicyName != "envplane-pvc-copy-fence-"+strings.TrimPrefix(d, "sha256:")[:32] || binding.Metadata.Annotations[fenceDigestAnnotation] != d {
			return fmt.Errorf("%w: reviewed source fence UID/spec drift", ErrUnsafe)
		}
		if policy.Metadata.Generation < 1 || policy.Status.ObservedGeneration != policy.Metadata.Generation || policy.Status.TypeChecking == nil || len(policy.Status.TypeChecking.ExpressionWarnings) > 0 {
			return fmt.Errorf("%w: reviewed source fence acceptance stale", ErrUnsafe)
		}
		for _, c := range policy.Status.Conditions {
			if c.Type == "Accepted" && (c.Status != "True" || c.ObservedGeneration != policy.Metadata.Generation) {
				return fmt.Errorf("%w: reviewed source fence rejected", ErrUnsafe)
			}
		}
		var ps map[string]any
		var bs map[string]any
		_ = json.Unmarshal(policy.Spec, &ps)
		_ = json.Unmarshal(binding.Spec, &bs)
		if ps["failurePolicy"] != "Fail" || ps["paramKind"] != nil || ps["matchConditions"] != nil || bs["policyName"] != ref.PolicyName {
			return fmt.Errorf("%w: reviewed source fence exclusions unsupported", ErrUnsafe)
		}
		actions, _ := bs["validationActions"].([]any)
		if len(actions) != 1 || actions[0] != "Deny" {
			return fmt.Errorf("%w: reviewed source fence not enforcing", ErrUnsafe)
		}
		// The sealed reviewed policy must protect every current source PVC for ALL
		// callers and bind the exact source UID helper and authenticated principal.
		consumer := "!(request.namespace == " + fmt.Sprintf("%q", item.SourceNamespace) + " && has(object.spec.volumes) && object.spec.volumes.exists(v, has(v.persistentVolumeClaim) && v.persistentVolumeClaim.claimName == " + fmt.Sprintf("%q", item.SourceName) + ")) || ("
		found := false
		validations, _ := ps["validations"].([]any)
		for _, row := range validations {
			v, _ := row.(map[string]any)
			expr, _ := v["expression"].(string)
			if strings.HasPrefix(expr, consumer) && strings.Contains(expr, fmt.Sprintf("%q", SourceHelperName(PVCRef{Namespace: item.SourceNamespace, Name: item.SourceName, UID: item.SourceUID}))) && strings.Contains(expr, fmt.Sprintf("%q", p.Image)) && strings.Contains(expr, fmt.Sprintf("%q", "system:serviceaccount:"+runnerNS+":"+runnerSA)) && v["message"] == fenceMessage(d) {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("%w: reviewed source fence does not bind plan source", ErrUnsafe)
		}
		checked[ref.PolicyName] = d
	}
	// Each challenge names only the exact helper; denial must identify the reviewed
	// policy/digest, not an unrelated policy or missing RBAC permission.
	for name, d := range checked {
		subset := p
		subset.Items = nil
		for _, item := range p.Items {
			for _, ref := range refs {
				if ref.Namespace == item.SourceNamespace && ref.PolicyName == name {
					subset.Items = append(subset.Items, item)
				}
			}
		}
		if err := k.probeAdmissionFence(ctx, subset, name, d); err != nil {
			return err
		}
	}
	return nil
}
