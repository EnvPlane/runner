package pvccopy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/envplane/contracts/domain"
)

const fenceAPIVersion = "admissionregistration.k8s.io/v1"
const fenceDigestAnnotation = "envplane.io/pvc-copy-fence-digest"

var fenceNamespace = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
var fenceScopeID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*(/[A-Za-z0-9][A-Za-z0-9._:-]*)*$`)
var fenceSourceUID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,127}$`)
var fenceIdentityDigest = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type fenceSourceAuthority struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
	Identity  string `json:"identity"`
}

// FenceNames returns the exact cluster-scoped policy and binding names for an
// operator's resourceNames-limited reader profile. Their identity is source
// authority, not environment/target/revision/plan identity: onboarding the same
// source set and Runner once covers later feature environments. These names
// confer no plan authorization and do not replace runtime source UID checks.
// Only source-authority fields are required, so operators can render the reader
// profile before any environment, target, template revision or plan exists.
func FenceNames(plan domain.PVCCopyPlan, runnerNamespace, runnerServiceAccount string) (policyName, bindingName string, err error) {
	name, _, err := fenceAuthority(plan, runnerNamespace, runnerServiceAccount)
	if err != nil {
		return "", "", err
	}
	return name, name, nil
}

func fenceAuthority(plan domain.PVCCopyPlan, runnerNamespace, runnerServiceAccount string) (name, digest string, err error) {
	if len(plan.TenantID) > 512 || !fenceScopeID.MatchString(plan.TenantID) || len(plan.ProjectID) > 512 || !fenceScopeID.MatchString(plan.ProjectID) || !imageRef.MatchString(plan.Image) || len(plan.Items) == 0 {
		return "", "", fmt.Errorf("%w: invalid admission fence source authority", ErrUnsafe)
	}
	if len(runnerNamespace) > 63 || !fenceNamespace.MatchString(runnerNamespace) || len(runnerServiceAccount) > 253 || !dnsName.MatchString(runnerServiceAccount) {
		return "", "", fmt.Errorf("%w: invalid admission fence Runner identity", ErrUnsafe)
	}
	for _, label := range strings.Split(runnerServiceAccount, ".") {
		if len(label) > 63 || !fenceNamespace.MatchString(label) {
			return "", "", fmt.Errorf("%w: invalid admission fence Runner service account", ErrUnsafe)
		}
	}
	sources := make([]fenceSourceAuthority, 0, len(plan.Items))
	seen := map[string]bool{}
	for _, item := range plan.Items {
		ref := item.SourceNamespace + "/" + item.SourceName
		if item.SourceTenantID != plan.TenantID || len(item.SourceNamespace) > 63 || !fenceNamespace.MatchString(item.SourceNamespace) || !fenceClaimName(item.SourceName) || !fenceSourceUID.MatchString(item.SourceUID) || !fenceIdentityDigest.MatchString(item.SourceIdentity) || seen[ref] {
			return "", "", fmt.Errorf("%w: invalid or ambiguous admission fence source reference", ErrUnsafe)
		}
		seen[ref] = true
		sources = append(sources, fenceSourceAuthority{item.SourceNamespace, item.SourceName, item.SourceUID, item.SourceIdentity})
	}
	sort.Slice(sources, func(i, j int) bool {
		return sources[i].Namespace+"/"+sources[i].Name < sources[j].Namespace+"/"+sources[j].Name
	})
	// A dedicated versioned projection prevents unrelated per-environment plan
	// fields or item IDs/bounds/order from entering the admission/RBAC identity.
	authority := struct {
		Version              string                 `json:"version"`
		TenantID             string                 `json:"tenantId"`
		ProjectID            string                 `json:"projectId"`
		RunnerNamespace      string                 `json:"runnerNamespace"`
		RunnerServiceAccount string                 `json:"runnerServiceAccount"`
		Image                string                 `json:"image"`
		Sources              []fenceSourceAuthority `json:"sources"`
	}{"v1", plan.TenantID, plan.ProjectID, runnerNamespace, runnerServiceAccount, plan.Image, sources}
	raw, err := json.Marshal(authority)
	if err != nil {
		return "", "", fmt.Errorf("%w: admission fence authority encoding failed", ErrUnsafe)
	}
	hash := sha256.Sum256(raw)
	// The full digest binds annotations and denial evidence; a 128-bit name
	// suffix keeps the complete policy name within a DNS label's 63-byte bound.
	return "envplane-pvc-copy-fence-" + hex.EncodeToString(hash[:16]), "sha256:" + hex.EncodeToString(hash[:]), nil
}

func fenceClaimName(name string) bool {
	if len(name) > 253 || name == "" {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if len(label) > 63 || !fenceNamespace.MatchString(label) {
			return false
		}
	}
	return true
}

// FenceManifests returns administrator-installable manifests, never applies
// them. Only source claims are fenced: the operator must hold this policy and
// binding throughout maintenance. The authenticated Runner may create only a
// pinned, tokenless, read-only source helper using the namespace's default SA
// (no helper credentials or new RBAC). A label alone is not authority.
// Target exclusivity is NOT admission proof: it requires the caller's durable
// prepublication gate/lease and the executor's consumer and partial-marker
// checks. Only source-authority fields are required for onboarding/rendering;
// this fence neither authorizes a feature plan nor marks an environment Ready.
func FenceManifests(plan domain.PVCCopyPlan, runnerNamespace, runnerServiceAccount string) (policy, binding map[string]any, err error) {
	name, digest, err := fenceAuthority(plan, runnerNamespace, runnerServiceAccount)
	if err != nil {
		return nil, nil, err
	}
	username := "system:serviceaccount:" + runnerNamespace + ":" + runnerServiceAccount
	common := strings.Join([]string{
		"request.operation == 'CREATE'",
		"(!has(request.subResource) || request.subResource == '')",
		"request.userInfo.username == " + strconv.Quote(username),
		"has(object.metadata.labels) && 'app.kubernetes.io/component' in object.metadata.labels && object.metadata.labels['app.kubernetes.io/component'] == 'pvccopy-helper'",
		"has(object.metadata.name)",
		"has(object.spec.serviceAccountName) && object.spec.serviceAccountName == 'default'",
		"has(object.spec.automountServiceAccountToken) && object.spec.automountServiceAccountToken == false",
		// Kubelet applies volume ownership/relabeling before a helper readback
		// can run. Reject metadata-changing contexts in admission, not after
		// scheduling; a read-only container mount is not sufficient protection.
		"has(object.spec.securityContext)",
		"!has(object.spec.securityContext.fsGroup) && !has(object.spec.securityContext.fsGroupChangePolicy)",
		"!has(object.spec.securityContext.supplementalGroups) && !has(object.spec.securityContext.supplementalGroupsPolicy)",
		"!has(object.spec.securityContext.seLinuxOptions) && !has(object.spec.securityContext.seLinuxChangePolicy)",
		"!has(object.spec.securityContext.sysctls) && !has(object.spec.securityContext.windowsOptions)",
		"!has(object.spec.securityContext.appArmorProfile)",
		"has(object.spec.securityContext.seccompProfile) && object.spec.securityContext.seccompProfile.type == 'RuntimeDefault' && !has(object.spec.securityContext.seccompProfile.localhostProfile)",
		"has(object.spec.securityContext.runAsUser) && has(object.spec.securityContext.runAsGroup) && has(object.spec.securityContext.runAsNonRoot)",
		"((object.spec.securityContext.runAsUser == 0 && object.spec.securityContext.runAsGroup == 0 && object.spec.securityContext.runAsNonRoot == false) || (object.spec.securityContext.runAsUser == 10001 && object.spec.securityContext.runAsGroup == 10001 && object.spec.securityContext.runAsNonRoot == true))",
		// Old profile annotations must not bypass the explicit seccomp and
		// AppArmor schema. Reject them even when a webhook injects them.
		"(!has(object.metadata.annotations) || object.metadata.annotations.all(k, k != 'seccomp.security.alpha.kubernetes.io/pod' && !k.startsWith('container.seccomp.security.alpha.kubernetes.io/') && !k.startsWith('container.apparmor.security.beta.kubernetes.io/')))",
		"object.spec.restartPolicy == 'Never'",
		"has(object.spec.activeDeadlineSeconds) && object.spec.activeDeadlineSeconds >= 31 && object.spec.activeDeadlineSeconds <= " + strconv.FormatInt(int64(MaxCopyTimeout/time.Second)+30, 10),
		"object.spec.terminationGracePeriodSeconds == 5",
		"(!has(object.spec.hostNetwork) || object.spec.hostNetwork == false)",
		"(!has(object.spec.hostPID) || object.spec.hostPID == false)",
		"(!has(object.spec.hostIPC) || object.spec.hostIPC == false)",
		"(!has(object.spec.initContainers) || size(object.spec.initContainers) == 0)",
		"(!has(object.spec.ephemeralContainers) || size(object.spec.ephemeralContainers) == 0)",
		"size(object.spec.containers) == 1",
		"object.spec.containers[0].name == 'copy'",
		"object.spec.containers[0].image == " + strconv.Quote(plan.Image),
		"has(object.spec.containers[0].command) && object.spec.containers[0].command == ['sleep']",
		"has(object.spec.containers[0].args) && size(object.spec.containers[0].args) == 1 && object.spec.containers[0].args[0].matches('^[0-9]{1,6}$') && int(object.spec.containers[0].args[0]) == object.spec.activeDeadlineSeconds",
		"!has(object.spec.containers[0].workingDir)",
		"(!has(object.spec.containers[0].terminationMessagePath) || object.spec.containers[0].terminationMessagePath == '/dev/termination-log')",
		"(!has(object.spec.containers[0].env) || size(object.spec.containers[0].env) == 0)",
		"(!has(object.spec.containers[0].envFrom) || size(object.spec.containers[0].envFrom) == 0)",
		"!has(object.spec.containers[0].lifecycle)",
		"!has(object.spec.containers[0].livenessProbe) && !has(object.spec.containers[0].readinessProbe) && !has(object.spec.containers[0].startupProbe)",
		"has(object.spec.containers[0].securityContext)",
		"!has(object.spec.containers[0].securityContext.seLinuxOptions)",
		"!has(object.spec.containers[0].securityContext.runAsUser) && !has(object.spec.containers[0].securityContext.runAsGroup) && !has(object.spec.containers[0].securityContext.runAsNonRoot)",
		"!has(object.spec.containers[0].securityContext.seccompProfile) && !has(object.spec.containers[0].securityContext.appArmorProfile)",
		"!has(object.spec.containers[0].securityContext.procMount) && !has(object.spec.containers[0].securityContext.windowsOptions)",
		"(!has(object.spec.containers[0].securityContext.privileged) || object.spec.containers[0].securityContext.privileged == false)",
		"object.spec.containers[0].securityContext.allowPrivilegeEscalation == false && object.spec.containers[0].securityContext.readOnlyRootFilesystem == true",
		"has(object.spec.containers[0].securityContext.capabilities) && object.spec.containers[0].securityContext.capabilities.drop == ['ALL']",
		"((object.spec.securityContext.runAsUser == 0 && has(object.spec.containers[0].securityContext.capabilities.add) && object.spec.containers[0].securityContext.capabilities.add == ['DAC_OVERRIDE']) || (object.spec.securityContext.runAsUser == 10001 && (!has(object.spec.containers[0].securityContext.capabilities.add) || size(object.spec.containers[0].securityContext.capabilities.add) == 0)))",
		"size(object.spec.volumes) == 1 && object.spec.volumes[0].name == 'data'",
		"has(object.spec.volumes[0].persistentVolumeClaim) && has(object.spec.volumes[0].persistentVolumeClaim.readOnly) && object.spec.volumes[0].persistentVolumeClaim.readOnly == true",
		"has(object.spec.containers[0].volumeMounts) && size(object.spec.containers[0].volumeMounts) == 1",
		"object.spec.containers[0].volumeMounts[0].name == 'data' && object.spec.containers[0].volumeMounts[0].mountPath == '/data'",
		"has(object.spec.containers[0].volumeMounts[0].readOnly) && object.spec.containers[0].volumeMounts[0].readOnly == true",
		"has(object.spec.containers[0].volumeMounts[0].recursiveReadOnly) && object.spec.containers[0].volumeMounts[0].recursiveReadOnly == 'Enabled'",
		"(!has(object.spec.containers[0].volumeDevices) || size(object.spec.containers[0].volumeDevices) == 0)",
		"(!has(object.spec.containers[0].volumeMounts[0].subPath) || object.spec.containers[0].volumeMounts[0].subPath == '')",
		"(!has(object.spec.containers[0].volumeMounts[0].subPathExpr) || object.spec.containers[0].volumeMounts[0].subPathExpr == '')",
		"(!has(object.spec.containers[0].volumeMounts[0].mountPropagation) || object.spec.containers[0].volumeMounts[0].mountPropagation == 'None')",
	}, " && ")
	items := append([]domain.PVCCopyItem(nil), plan.Items...)
	sort.Slice(items, func(i, j int) bool {
		return items[i].SourceNamespace+"/"+items[i].SourceName < items[j].SourceNamespace+"/"+items[j].SourceName
	})
	namespaceSet := map[string]bool{}
	helperBindings := map[string][]string{}
	validations := make([]any, 0, len(items))
	for _, item := range items {
		namespaceSet[item.SourceNamespace] = true
		claim := strconv.Quote(item.SourceName)
		helper := "object.metadata.name == " + strconv.Quote(SourceHelperName(PVCRef{Namespace: item.SourceNamespace, Name: item.SourceName, UID: item.SourceUID})) + " && object.spec.volumes[0].persistentVolumeClaim.claimName == " + claim
		helperBindings[item.SourceNamespace] = append(helperBindings[item.SourceNamespace], "("+helper+")")
		// request.namespace is authoritative, not a mutable object label.
		consumer := "request.namespace == " + strconv.Quote(item.SourceNamespace) + " && has(object.spec.volumes) && object.spec.volumes.exists(v, has(v.persistentVolumeClaim) && v.persistentVolumeClaim.claimName == " + claim + ")"
		expression := "!(" + consumer + ") || (" + common + " && " + helper + ")"
		validations = append(validations, map[string]any{"expression": expression, "message": fenceMessage(digest)})
	}
	namespaces := make([]string, 0, len(namespaceSet))
	for ns := range namespaceSet {
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)
	for _, ns := range namespaces {
		// Source Pod CREATE privilege must not become a path to mount other
		// PVCs/Secrets: even an unrelated Pod created by this Runner must be
		// one of the exact allowed tokenless source helpers in this namespace.
		expression := "request.operation != 'CREATE' || request.userInfo.username != " + strconv.Quote(username) + " || request.namespace != " + strconv.Quote(ns) + " || (" + common + " && (" + strings.Join(helperBindings[ns], " || ") + "))"
		validations = append(validations, map[string]any{"expression": expression, "message": fenceMessage(digest)})
	}
	annotations := map[string]any{fenceDigestAnnotation: digest}
	policy = map[string]any{
		"apiVersion": fenceAPIVersion, "kind": "ValidatingAdmissionPolicy",
		"metadata": map[string]any{"name": name, "annotations": annotations},
		"spec": map[string]any{
			"failurePolicy": "Fail",
			"matchConstraints": map[string]any{
				"matchPolicy": "Exact", "namespaceSelector": map[string]any{}, "objectSelector": map[string]any{},
				// Include ephemeral-container updates to prevent injecting a
				// writer into a protected helper through the Pod subresource.
				"resourceRules": []any{map[string]any{"apiGroups": []string{""}, "apiVersions": []string{"v1"}, "operations": []string{"CREATE", "UPDATE"}, "resources": []string{"pods", "pods/ephemeralcontainers"}, "scope": "Namespaced"}},
			},
			"validations": validations,
		},
	}
	binding = map[string]any{
		"apiVersion": fenceAPIVersion, "kind": "ValidatingAdmissionPolicyBinding",
		"metadata": map[string]any{"name": name, "annotations": map[string]any{fenceDigestAnnotation: digest}},
		"spec": map[string]any{
			"policyName": name, "validationActions": []string{"Deny"},
			"matchResources": map[string]any{
				"matchPolicy": "Exact", "objectSelector": map[string]any{},
				"namespaceSelector": map[string]any{"matchExpressions": []any{map[string]any{"key": "kubernetes.io/metadata.name", "operator": "In", "values": namespaces}}},
			},
		},
	}
	return policy, binding, nil
}

type fenceObject struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name              string            `json:"name"`
		Namespace         string            `json:"namespace"`
		UID               string            `json:"uid"`
		Generation        int64             `json:"generation"`
		DeletionTimestamp *string           `json:"deletionTimestamp"`
		Annotations       map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec   json.RawMessage `json:"spec"`
	Status struct {
		ObservedGeneration int64 `json:"observedGeneration"`
		TypeChecking       *struct {
			ExpressionWarnings []json.RawMessage `json:"expressionWarnings"`
		} `json:"typeChecking"`
		Conditions []struct {
			Type               string `json:"type"`
			Status             string `json:"status"`
			ObservedGeneration int64  `json:"observedGeneration"`
		} `json:"conditions"`
	} `json:"status"`
}

// RequireAdmissionFence reads only the exact generated policy and binding.
// Negative server-side dry-runs prove enforcement for each source at this instant,
// including rejection of read-only helpers with volume metadata injections,
// not that an administrator cannot delete the policy afterward. The owner must
// hold it installed; the parent must recheck at every validation stage, and its
// positive helper preflight must independently prove helper admission/RBAC.
// Plan authorization, source UID validation and existing-consumer checks remain
// separate mandatory boundaries. No claim annotation is accepted as a fence.
func (k *Kubectl) RequireAdmissionFence(ctx context.Context, plan domain.PVCCopyPlan, runnerNamespace, runnerServiceAccount string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := plan.Validate(); err != nil {
		return fmt.Errorf("%w: invalid admission fence execution plan", ErrUnsafe)
	}
	policy, binding, err := FenceManifests(plan, runnerNamespace, runnerServiceAccount)
	if err != nil {
		return err
	}
	if k == nil {
		return fmt.Errorf("%w: admission fence transport unavailable", ErrUnsafe)
	}
	name := policy["metadata"].(map[string]any)["name"].(string)
	digest := policy["metadata"].(map[string]any)["annotations"].(map[string]any)[fenceDigestAnnotation].(string)
	for i, expected := range []map[string]any{policy, binding} {
		resource := "validatingadmissionpolicies.admissionregistration.k8s.io"
		if i == 1 {
			resource = "validatingadmissionpolicybindings.admissionregistration.k8s.io"
		}
		var actual fenceObject
		if err := k.json(ctx, []string{"get", resource, name, "-o", "json", "--request-timeout=30s"}, nil, &actual); err != nil {
			// Do not expose arbitrary kubectl output or response content.
			return fmt.Errorf("%w: administrator-provisioned admission fence unavailable", ErrUnsafe)
		}
		if actual.APIVersion != fenceAPIVersion || actual.Kind != expected["kind"] || actual.Metadata.Name != name || actual.Metadata.Namespace != "" || actual.Metadata.UID == "" || actual.Metadata.Generation <= 0 || actual.Metadata.DeletionTimestamp != nil || actual.Metadata.Annotations[fenceDigestAnnotation] != digest {
			return fmt.Errorf("%w: admission fence identity drift", ErrUnsafe)
		}
		var gotSpec, wantSpec any
		wantJSON, err := json.Marshal(expected["spec"])
		if err != nil || json.Unmarshal(actual.Spec, &gotSpec) != nil || json.Unmarshal(wantJSON, &wantSpec) != nil || !reflect.DeepEqual(gotSpec, wantSpec) {
			return fmt.Errorf("%w: admission fence specification drift", ErrUnsafe)
		}
		if i == 0 {
			// Upstream VAP reports typeChecking/observedGeneration, not an
			// Accepted condition. Require its real completion signal, and fail
			// closed on nonaccepted/unknown/stale vendor conditions if present.
			if actual.Status.ObservedGeneration != actual.Metadata.Generation || actual.Status.TypeChecking == nil || len(actual.Status.TypeChecking.ExpressionWarnings) != 0 {
				return fmt.Errorf("%w: admission fence has not passed current-generation type checking", ErrUnsafe)
			}
			for _, condition := range actual.Status.Conditions {
				if condition.Type != "Accepted" || condition.Status != "True" || condition.ObservedGeneration != actual.Metadata.Generation {
					return fmt.Errorf("%w: admission fence is not accepted", ErrUnsafe)
				}
			}
		}
	}
	return k.probeAdmissionFence(ctx, plan, name, digest)
}

func fenceMessage(digest string) string {
	return "envplane PVC copy source fence [" + digest + "]"
}

// AdmissionFenceProbeRunner permits synthetic denial evidence in tests without
// changing the shared CommandRunner (which deliberately discards stderr).
// Implementations must never persist the Pod; args mandate --dry-run=server.
// The output writer is bounded and its contents never escape in returned errors.
type AdmissionFenceProbeRunner interface {
	RunAdmissionFenceProbe(context.Context, []string, io.Reader, io.Writer) error
}

type fenceProbeOutput struct {
	bytes.Buffer
	overflow bool
}

func (b *fenceProbeOutput) Write(p []byte) (int, error) {
	if b.overflow || len(p) > (16<<10)-b.Len() {
		b.overflow = true
		return len(p), nil // Drain child stderr without retaining unbounded data.
	}
	return b.Buffer.Write(p)
}

func (k *Kubectl) probeAdmissionFence(ctx context.Context, plan domain.PVCCopyPlan, name, digest string) error {
	expected := fmt.Sprintf("ValidatingAdmissionPolicy '%s' with binding '%s' denied request: %s", name, name, fenceMessage(digest))
	for _, item := range plan.Items {
		for _, probeKind := range []string{"writable", "fsGroup", "seLinux"} {
			// Probe the exact helper identity/schema except for one unsafe field.
			// This tests injection defenses before scheduling, not just RW denial.
			// No digest message or policy name is included in the submitted object,
			// so an echoed request body cannot be mistaken for denial evidence.
			readOnly := probeKind != "writable"
			mount := map[string]any{"name": "data", "mountPath": "/data", "readOnly": readOnly}
			if readOnly {
				mount["recursiveReadOnly"] = "Enabled"
			}
			pod := map[string]any{
				"apiVersion": "v1", "kind": "Pod",
				"metadata": map[string]any{"name": SourceHelperName(PVCRef{Namespace: item.SourceNamespace, Name: item.SourceName, UID: item.SourceUID}), "namespace": item.SourceNamespace, "labels": map[string]any{"app.kubernetes.io/component": "pvccopy-helper"}},
				"spec": map[string]any{
					"serviceAccountName": "default", "automountServiceAccountToken": false, "restartPolicy": "Never", "activeDeadlineSeconds": 60, "terminationGracePeriodSeconds": 5,
					"securityContext": map[string]any{"runAsNonRoot": true, "runAsUser": 10001, "runAsGroup": 10001, "seccompProfile": map[string]any{"type": "RuntimeDefault"}},
					"containers": []any{map[string]any{"name": "copy", "image": plan.Image, "command": []string{"sleep"}, "args": []string{"60"},
						"securityContext": map[string]any{"allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": map[string]any{"drop": []string{"ALL"}}},
						"resources":       map[string]any{"requests": map[string]string{"cpu": "10m", "memory": "32Mi"}, "limits": map[string]string{"cpu": "1", "memory": "256Mi"}},
						"volumeMounts":    []any{mount}}},
					"volumes": []any{map[string]any{"name": "data", "persistentVolumeClaim": map[string]any{"claimName": item.SourceName, "readOnly": readOnly}}},
				},
			}
			security := pod["spec"].(map[string]any)["securityContext"].(map[string]any)
			switch probeKind {
			case "fsGroup":
				security["fsGroup"] = 10001
			case "seLinux":
				security["seLinuxOptions"] = map[string]any{"type": "container_t"}
			}
			input, err := json.Marshal(pod)
			if err != nil {
				return fmt.Errorf("%w: admission fence probe encoding failed", ErrUnsafe)
			}
			args := []string{"create", "--dry-run=server", "-f", "-", "-n", item.SourceNamespace, "-o", "json", "--request-timeout=20s"}
			probeCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
			var output fenceProbeOutput
			if k.Commands != nil {
				probe, ok := k.Commands.(AdmissionFenceProbeRunner)
				if !ok {
					cancel()
					return fmt.Errorf("%w: admission fence probe transport unavailable", ErrUnsafe)
				}
				err = probe.RunAdmissionFenceProbe(probeCtx, args, bytes.NewReader(input), &output)
			} else {
				cmd := exec.CommandContext(probeCtx, "kubectl", args...)
				cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(input), io.Discard, &output
				cmd.WaitDelay = 2 * time.Second
				err = cmd.Run()
			}
			canceled := probeCtx.Err() != nil
			cancel()
			denied := false
			for _, line := range strings.Split(output.String(), "\n") {
				// Upstream emits this exact policy/binding denial segment. Require
				// its fixed digest message at line end, not arbitrary name substrings.
				if strings.HasSuffix(strings.TrimSpace(line), expected) {
					denied = true
				}
			}
			if err == nil || canceled || output.overflow || !denied {
				return fmt.Errorf("%w: source admission fence enforcement not proven", ErrUnsafe)
			}
		}
	}
	return nil
}
