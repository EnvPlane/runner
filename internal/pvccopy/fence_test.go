package pvccopy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/envplane/contracts/domain"
)

func fenceTestPlan(t *testing.T) domain.PVCCopyPlan {
	t.Helper()
	p := domain.PVCCopyPlan{
		ContractVersion: domain.PVCCopyContractVersion, PlanID: "plan-fence", TenantID: "tenant", ProjectID: "project", EnvironmentID: "environment",
		TemplateRevisionID: "revision", TemplateDigest: "sha256:" + strings.Repeat("a", 64), TargetNamespace: "preview",
		AllowedSourceNamespaces: []string{"source-b", "source-a"}, Image: "example.test/runner@sha256:" + strings.Repeat("b", 64),
		MaxBytes: 2048, StorageQuotaBytes: 2048, TimeoutSeconds: 60,
		Items: []domain.PVCCopyItem{
			{ID: "b", SourceTenantID: "tenant", SourceNamespace: "source-b", SourceName: "database-b", SourceUID: "uid-b", SourceIdentity: "sha256:" + strings.Repeat("c", 64), SourceRequestedBytes: 1024, TargetName: "target-b", StorageClass: "local-path", AccessModes: []string{"ReadWriteOnce"}, RequestedBytes: 1024, Method: domain.PVCCopyFilesystemOffline, MaxBytes: 1024, TimeoutSeconds: 60},
			{ID: "a", SourceTenantID: "tenant", SourceNamespace: "source-a", SourceName: "database-a", SourceUID: "uid-a", SourceIdentity: "sha256:" + strings.Repeat("d", 64), SourceRequestedBytes: 1024, TargetName: "target-a", StorageClass: "local-path", AccessModes: []string{"ReadWriteOnce"}, RequestedBytes: 1024, Method: domain.PVCCopyFilesystemOffline, MaxBytes: 1024, TimeoutSeconds: 60},
		},
	}
	var err error
	p.Digest, err = p.CanonicalDigest()
	if err != nil || p.Validate() != nil {
		t.Fatalf("fixture invalid: %v", err)
	}
	return p
}

func fenceTestObjects(t *testing.T, p domain.PVCCopyPlan) (map[string]any, map[string]any) {
	t.Helper()
	policy, binding, err := FenceManifests(p, "runner-ns", "runtime-runner")
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256([]byte(p.Digest))
	expectedName := "envplane-pvc-copy-fence-" + hex.EncodeToString(hash[:16])
	if policy["metadata"].(map[string]any)["name"] != expectedName || len(expectedName) > 63 {
		t.Fatal("policy name is not bound to the immutable digest")
	}
	// Normalize through JSON just as the Kubernetes wire response does.
	clone := func(obj map[string]any) map[string]any {
		raw, _ := json.Marshal(obj)
		var result map[string]any
		_ = json.Unmarshal(raw, &result)
		meta := result["metadata"].(map[string]any)
		meta["uid"], meta["generation"] = "uid-policy", float64(3)
		return result
	}
	policy, binding = clone(policy), clone(binding)
	policy["status"] = map[string]any{"observedGeneration": float64(3), "typeChecking": map[string]any{}}
	return policy, binding
}

type fenceTestCommands struct {
	t       *testing.T
	policy  map[string]any
	binding map[string]any
	reads   int
	probes  int
	readErr error
	probe   func(context.Context, []string, map[string]any, io.Writer) error
}

func (f *fenceTestCommands) Run(ctx context.Context, args []string, input io.Reader, out io.Writer) error {
	f.t.Helper()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if input != nil || len(args) != 6 || args[0] != "get" || args[3] != "-o" || args[4] != "json" || args[5] != "--request-timeout=30s" {
		f.t.Fatalf("non-read metadata operation: %v", args)
	}
	f.reads++
	obj := f.policy
	if args[1] == "validatingadmissionpolicybindings.admissionregistration.k8s.io" {
		obj = f.binding
	} else if args[1] != "validatingadmissionpolicies.admissionregistration.k8s.io" {
		f.t.Fatalf("unexpected resource: %v", args)
	}
	if args[2] != obj["metadata"].(map[string]any)["name"] {
		f.t.Fatal("policy name was not derived from the plan")
	}
	if f.readErr != nil {
		return f.readErr
	}
	return json.NewEncoder(out).Encode(obj)
}

func (f *fenceTestCommands) RunAdmissionFenceProbe(ctx context.Context, args []string, input io.Reader, out io.Writer) error {
	f.t.Helper()
	if len(args) != 9 || !reflect.DeepEqual(args, []string{"create", "--dry-run=server", "-f", "-", "-n", args[5], "-o", "json", "--request-timeout=20s"}) {
		f.t.Fatalf("unsafe probe argv: %v", args)
	}
	var pod map[string]any
	if err := json.NewDecoder(input).Decode(&pod); err != nil {
		f.t.Fatal(err)
	}
	if pod["metadata"].(map[string]any)["namespace"] != args[5] {
		f.t.Fatal("probe namespace mismatch")
	}
	spec := pod["spec"].(map[string]any)
	volume := spec["volumes"].([]any)[0].(map[string]any)["persistentVolumeClaim"].(map[string]any)
	if volume["readOnly"] != false || spec["automountServiceAccountToken"] != false || spec["serviceAccountName"] != "default" {
		f.t.Fatal("probe must be writable but tokenless")
	}
	f.probes++
	if f.probe != nil {
		return f.probe(ctx, args, pod, out)
	}
	name := f.policy["metadata"].(map[string]any)["name"].(string)
	message := f.policy["spec"].(map[string]any)["validations"].([]any)[0].(map[string]any)["message"].(string)
	_, _ = io.WriteString(out, "Error from server (Invalid): pods forbidden: ValidatingAdmissionPolicy '"+name+"' with binding '"+name+"' denied request: "+message+"\n")
	return errors.New("exit status 1")
}

func TestFenceManifestsCanonicalSourceOnly(t *testing.T) {
	p := fenceTestPlan(t)
	policy, binding, err := FenceManifests(p, "runner-ns", "runtime-runner")
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(p)
	if _, _, err := FenceManifests(p, "runner-ns", "runtime-runner"); err != nil {
		t.Fatal(err)
	}
	unmutated, _ := json.Marshal(p)
	if !reflect.DeepEqual(before, unmutated) {
		t.Fatal("generation mutated input")
	}
	p.Items[0], p.Items[1] = p.Items[1], p.Items[0]
	p.AllowedSourceNamespaces[0], p.AllowedSourceNamespaces[1] = p.AllowedSourceNamespaces[1], p.AllowedSourceNamespaces[0]
	reordered, reorderedBinding, err := FenceManifests(p, "runner-ns", "runtime-runner")
	if err != nil || !reflect.DeepEqual(policy, reordered) || !reflect.DeepEqual(binding, reorderedBinding) {
		t.Fatal("fence manifests are not canonical")
	}
	after, _ := json.Marshal(fenceTestPlan(t))
	if !reflect.DeepEqual(before, after) {
		t.Fatal("generation mutated input")
	}
	raw, _ := json.Marshal(policy["spec"])
	for _, required := range []string{"system:serviceaccount:runner-ns:runtime-runner", "serviceAccountName == 'default'", "readOnly == true", "recursiveReadOnly == 'Enabled'", "automountServiceAccountToken == false", "size(object.spec.containers) == 1", "DAC_OVERRIDE", "database-a", "database-b", "source-a", "source-b", p.Image, p.Digest, "pods/ephemeralcontainers"} {
		if !strings.Contains(string(raw), required) {
			t.Errorf("missing fence constraint %q", required)
		}
	}
	for _, forbidden := range []string{"target-a", "target-b", "preview", "CHOWN", "FOWNER"} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("source fence contains %q", forbidden)
		}
	}
	validations := policy["spec"].(map[string]any)["validations"].([]any)
	if len(validations) != 4 {
		t.Fatalf("missing per-source/per-namespace constraints: %d", len(validations))
	}
	for _, item := range p.Items {
		name := SourceHelperName(PVCRef{Namespace: item.SourceNamespace, Name: item.SourceName, UID: item.SourceUID})
		if len(name) != len("pvccopy-source-")+24 || !strings.Contains(string(raw), name) {
			t.Fatal("helper identity is not pinned")
		}
	}
	for _, value := range validations[2:] {
		expression := value.(map[string]any)["expression"].(string)
		if !strings.Contains(expression, "request.operation != 'CREATE' || request.userInfo.username !=") || !strings.Contains(expression, "request.namespace !=") {
			t.Fatal("Runner arbitrary source Pod creation not fenced")
		}
	}
	selector := binding["spec"].(map[string]any)["matchResources"].(map[string]any)["namespaceSelector"].(map[string]any)["matchExpressions"].([]any)[0].(map[string]any)
	if !reflect.DeepEqual(selector["values"], []string{"source-a", "source-b"}) || selector["operator"] != "In" || selector["key"] != "kubernetes.io/metadata.name" {
		t.Fatal("binding does not select exact source namespaces")
	}
}

func TestSourceHelperNameBindsAllIdentityFields(t *testing.T) {
	ref := PVCRef{Namespace: "source-a", Name: "database-a", UID: "uid-a"}
	name := SourceHelperName(ref)
	if name != SourceHelperName(ref) {
		t.Fatal("unstable helper name")
	}
	for _, changed := range []PVCRef{{Namespace: "other", Name: ref.Name, UID: ref.UID}, {Namespace: ref.Namespace, Name: "other", UID: ref.UID}, {Namespace: ref.Namespace, Name: ref.Name, UID: "replacement"}} {
		if SourceHelperName(changed) == name {
			t.Fatal("helper name failed to bind namespace/name/UID")
		}
	}
}

func TestRequireAdmissionFencePassesOnlyCanonicalEnforcedPolicy(t *testing.T) {
	p := fenceTestPlan(t)
	policy, binding := fenceTestObjects(t, p)
	f := &fenceTestCommands{t: t, policy: policy, binding: binding}
	if err := (&Kubectl{Commands: f}).RequireAdmissionFence(context.Background(), p, "runner-ns", "runtime-runner"); err != nil {
		t.Fatal(err)
	}
	if f.reads != 2 || f.probes != len(p.Items) {
		t.Fatalf("missing metadata reads/source probes: %d/%d", f.reads, f.probes)
	}
}

func TestRequireAdmissionFenceRejectsDriftBeforeProbe(t *testing.T) {
	cases := map[string]func(map[string]any, map[string]any){
		"missing UID": func(p, b map[string]any) { delete(p["metadata"].(map[string]any), "uid") },
		"deleting":    func(p, b map[string]any) { p["metadata"].(map[string]any)["deletionTimestamp"] = "now" },
		"wrong digest": func(p, b map[string]any) {
			b["metadata"].(map[string]any)["annotations"].(map[string]any)["envplane.io/pvc-copy-plan-digest"] = "forged"
		},
		"ignore failures": func(p, b map[string]any) { p["spec"].(map[string]any)["failurePolicy"] = "Ignore" },
		"exclude pods": func(p, b map[string]any) {
			p["spec"].(map[string]any)["matchConstraints"].(map[string]any)["excludeResourceRules"] = []any{map[string]any{"resources": []string{"pods"}}}
		},
		"object selector": func(p, b map[string]any) {
			p["spec"].(map[string]any)["matchConstraints"].(map[string]any)["objectSelector"] = map[string]any{"matchLabels": map[string]any{"skip": "true"}}
		},
		"match conditions": func(p, b map[string]any) {
			p["spec"].(map[string]any)["matchConditions"] = []any{map[string]any{"name": "skip", "expression": "false"}}
		},
		"arbitrary CEL": func(p, b map[string]any) {
			p["spec"].(map[string]any)["validations"].([]any)[0].(map[string]any)["expression"] = "true"
		},
		"missing claim": func(p, b map[string]any) {
			p["spec"].(map[string]any)["validations"] = p["spec"].(map[string]any)["validations"].([]any)[:1]
		},
		"wrong principal": func(p, b map[string]any) {
			v := p["spec"].(map[string]any)["validations"].([]any)[0].(map[string]any)
			v["expression"] = strings.ReplaceAll(v["expression"].(string), "runtime-runner", "default")
		},
		"RW exception": func(p, b map[string]any) {
			v := p["spec"].(map[string]any)["validations"].([]any)[0].(map[string]any)
			v["expression"] = strings.ReplaceAll(v["expression"].(string), "readOnly == true", "readOnly == false")
		},
		"wrong message": func(p, b map[string]any) {
			p["spec"].(map[string]any)["validations"].([]any)[0].(map[string]any)["message"] = "arbitrary"
		},
		"warn only":      func(p, b map[string]any) { b["spec"].(map[string]any)["validationActions"] = []string{"Warn"} },
		"wrong policy":   func(p, b map[string]any) { b["spec"].(map[string]any)["policyName"] = "other" },
		"param override": func(p, b map[string]any) { b["spec"].(map[string]any)["paramRef"] = map[string]any{"name": "other"} },
		"binding selector": func(p, b map[string]any) {
			b["spec"].(map[string]any)["matchResources"].(map[string]any)["namespaceSelector"] = map[string]any{}
		},
		"missing status":    func(p, b map[string]any) { delete(p, "status") },
		"stale generation":  func(p, b map[string]any) { p["status"].(map[string]any)["observedGeneration"] = float64(2) },
		"missing typecheck": func(p, b map[string]any) { delete(p["status"].(map[string]any), "typeChecking") },
		"typecheck warning": func(p, b map[string]any) {
			p["status"].(map[string]any)["typeChecking"].(map[string]any)["expressionWarnings"] = []any{map[string]any{"warning": "unknown field"}}
		},
		"not accepted": func(p, b map[string]any) {
			p["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "Accepted", "status": "False", "observedGeneration": 3}}
		},
		"stale accepted": func(p, b map[string]any) {
			p["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "Accepted", "status": "True", "observedGeneration": 2}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := fenceTestPlan(t)
			policy, binding := fenceTestObjects(t, p)
			mutate(policy, binding)
			f := &fenceTestCommands{t: t, policy: policy, binding: binding}
			if err := (&Kubectl{Commands: f}).RequireAdmissionFence(context.Background(), p, "runner-ns", "runtime-runner"); !errors.Is(err, ErrUnsafe) || f.probes != 0 {
				t.Fatalf("drift accepted/probed: %v probes=%d", err, f.probes)
			}
		})
	}
}

func TestAdmissionFenceNegativeProbeEvidence(t *testing.T) {
	for _, mode := range []string{"admitted", "disabled plugin", "unrelated denial", "wrong policy", "wrong digest", "wrong binding", "overflow", "canceled", "second source unfenced", "successful exit with denial text"} {
		t.Run(mode, func(t *testing.T) {
			p := fenceTestPlan(t)
			policy, binding := fenceTestObjects(t, p)
			f := &fenceTestCommands{t: t, policy: policy, binding: binding}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f.probe = func(_ context.Context, _ []string, _ map[string]any, out io.Writer) error {
				name := policy["metadata"].(map[string]any)["name"].(string)
				text := "ValidatingAdmissionPolicy '" + name + "' with binding '" + name + "' denied request: " + fenceMessage(p)
				switch mode {
				case "admitted", "disabled plugin":
					return nil
				case "unrelated denial":
					text = "Forbidden by RBAC or PodSecurity; private payload"
				case "wrong policy":
					text = strings.Replace(text, "ValidatingAdmissionPolicy '"+name+"'", "ValidatingAdmissionPolicy 'other'", 1)
				case "wrong digest":
					text = strings.ReplaceAll(text, p.Digest, "sha256:"+strings.Repeat("e", 64))
				case "wrong binding":
					text = strings.Replace(text, "binding '"+name+"'", "binding 'other'", 1)
				case "overflow":
					text += strings.Repeat("private", 4096)
				case "canceled":
					cancel()
				case "second source unfenced":
					if f.probes == 2 {
						return nil
					}
				}
				_, _ = io.WriteString(out, text)
				if mode == "successful exit with denial text" {
					return nil
				}
				return errors.New("private raw stderr must not be returned")
			}
			err := (&Kubectl{Commands: f}).RequireAdmissionFence(ctx, p, "runner-ns", "runtime-runner")
			if !errors.Is(err, ErrUnsafe) || strings.Contains(err.Error(), "private") {
				t.Fatalf("unsafe evidence accepted or leaked: %v", err)
			}
		})
	}
}

func TestFenceRejectsInvalidInputsAndUnavailableReads(t *testing.T) {
	p := fenceTestPlan(t)
	for _, identity := range [][2]string{{"", "sa"}, {"runner.ns", "sa"}, {"runner-ns", "a..b"}, {"runner-ns", "sa' || true"}, {"runner-ns", ""}} {
		if _, _, err := FenceManifests(p, identity[0], identity[1]); !errors.Is(err, ErrUnsafe) {
			t.Fatal("invalid identity accepted")
		}
	}
	invalid := p
	invalid.Digest = "sha256:" + strings.Repeat("f", 64)
	if _, _, err := FenceManifests(invalid, "runner-ns", "runtime-runner"); !errors.Is(err, ErrUnsafe) {
		t.Fatal("tampered plan accepted")
	}
	policy, binding := fenceTestObjects(t, p)
	f := &fenceTestCommands{t: t, policy: policy, binding: binding, readErr: errors.New("private missing/forbidden/unsupported policy")}
	err := (&Kubectl{Commands: f}).RequireAdmissionFence(context.Background(), p, "runner-ns", "runtime-runner")
	if !errors.Is(err, ErrUnsafe) || strings.Contains(err.Error(), "private") || f.probes != 0 {
		t.Fatalf("read failure: %v", err)
	}
}

type fenceReadOnlyCommands struct{ base *fenceTestCommands }

func (f fenceReadOnlyCommands) Run(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	return f.base.Run(ctx, args, in, out)
}

func TestFenceRequiresProbeTransportAndCurrentAcceptedCondition(t *testing.T) {
	p := fenceTestPlan(t)
	policy, binding := fenceTestObjects(t, p)
	policy["status"].(map[string]any)["conditions"] = []any{map[string]any{"type": "Accepted", "status": "True", "observedGeneration": 3}}
	f := &fenceTestCommands{t: t, policy: policy, binding: binding}
	if err := (&Kubectl{Commands: fenceReadOnlyCommands{base: f}}).RequireAdmissionFence(context.Background(), p, "runner-ns", "runtime-runner"); !errors.Is(err, ErrUnsafe) {
		t.Fatal("metadata-only transport falsely proved enforcement")
	}
	if err := (&Kubectl{Commands: f}).RequireAdmissionFence(context.Background(), p, "runner-ns", "runtime-runner"); err != nil {
		t.Fatal(err)
	}
	var nilTransport *Kubectl
	if err := nilTransport.RequireAdmissionFence(context.Background(), p, "runner-ns", "runtime-runner"); !errors.Is(err, ErrUnsafe) {
		t.Fatal("nil transport accepted")
	}
}
