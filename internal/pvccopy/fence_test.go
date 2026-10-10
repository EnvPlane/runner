package pvccopy

import (
	"context"
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
	expectedName, bindingName, err := FenceNames(p, "runner-ns", "runtime-runner")
	if err != nil || policy["metadata"].(map[string]any)["name"] != expectedName || binding["metadata"].(map[string]any)["name"] != bindingName || len(expectedName) > 63 {
		t.Fatal("policy/binding names do not match the source authority projection")
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
		return errors.New("requested source-bound fence not found")
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
	security := spec["securityContext"].(map[string]any)
	container := spec["containers"].([]any)[0].(map[string]any)
	mount := container["volumeMounts"].([]any)[0].(map[string]any)
	if spec["automountServiceAccountToken"] != false || spec["serviceAccountName"] != "default" || mount["readOnly"] != volume["readOnly"] {
		f.t.Fatal("probe must be tokenless with consistent volume/mount flags")
	}
	if volume["readOnly"] == true {
		if mount["recursiveReadOnly"] != "Enabled" || (security["fsGroup"] == nil && security["seLinuxOptions"] == nil) {
			f.t.Fatal("read-only negative probe lacks the metadata injection")
		}
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
	fenceDigest := policy["metadata"].(map[string]any)["annotations"].(map[string]any)[fenceDigestAnnotation].(string)
	for _, required := range []string{"system:serviceaccount:runner-ns:runtime-runner", "serviceAccountName == 'default'", "readOnly == true", "recursiveReadOnly == 'Enabled'", "automountServiceAccountToken == false", "size(object.spec.containers) == 1", "DAC_OVERRIDE", "database-a", "database-b", "source-a", "source-b", p.Image, fenceDigest, "pods/ephemeralcontainers"} {
		if !strings.Contains(string(raw), required) {
			t.Errorf("missing fence constraint %q", required)
		}
	}
	for _, forbidden := range []string{"target-a", "target-b", "preview", "CHOWN", "FOWNER", p.Digest} {
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
	if f.reads != 2 || f.probes != 3*len(p.Items) {
		t.Fatalf("missing metadata reads/source probes: %d/%d", f.reads, f.probes)
	}
}

func TestRequireAdmissionFenceRejectsDriftBeforeProbe(t *testing.T) {
	cases := map[string]func(map[string]any, map[string]any){
		"missing UID": func(p, b map[string]any) { delete(p["metadata"].(map[string]any), "uid") },
		"deleting":    func(p, b map[string]any) { p["metadata"].(map[string]any)["deletionTimestamp"] = "now" },
		"wrong digest": func(p, b map[string]any) {
			b["metadata"].(map[string]any)["annotations"].(map[string]any)[fenceDigestAnnotation] = "forged"
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
				digest := policy["metadata"].(map[string]any)["annotations"].(map[string]any)[fenceDigestAnnotation].(string)
				text := "ValidatingAdmissionPolicy '" + name + "' with binding '" + name + "' denied request: " + fenceMessage(digest)
				switch mode {
				case "admitted", "disabled plugin":
					return nil
				case "unrelated denial":
					text = "Forbidden by RBAC or PodSecurity; private payload"
				case "wrong policy":
					text = strings.Replace(text, "ValidatingAdmissionPolicy '"+name+"'", "ValidatingAdmissionPolicy 'other'", 1)
				case "wrong digest":
					text = strings.ReplaceAll(text, digest, "sha256:"+strings.Repeat("e", 64))
				case "wrong binding":
					text = strings.Replace(text, "binding '"+name+"'", "binding 'other'", 1)
				case "overflow":
					text += strings.Repeat("private", 4096)
				case "canceled":
					cancel()
				case "second source unfenced":
					if f.probes == 4 {
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
	if err := (&Kubectl{}).RequireAdmissionFence(context.Background(), invalid, "runner-ns", "runtime-runner"); !errors.Is(err, ErrUnsafe) {
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

func fenceResealTestPlan(t *testing.T, p domain.PVCCopyPlan) domain.PVCCopyPlan {
	t.Helper()
	var err error
	p.Digest, err = p.CanonicalDigest()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Validate(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFenceSourceAuthorityReusedAcrossFeaturePlans(t *testing.T) {
	base := fenceTestPlan(t)
	policy, binding := fenceTestObjects(t, base)
	basePolicyName, baseBindingName, err := FenceNames(base, "runner-ns", "runtime-runner")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*domain.PVCCopyPlan){
		"environment":      func(p *domain.PVCCopyPlan) { p.EnvironmentID = "another-feature" },
		"target namespace": func(p *domain.PVCCopyPlan) { p.TargetNamespace = "another-preview" },
		"target claims":    func(p *domain.PVCCopyPlan) { p.Items[0].TargetName = "another-target" },
		"template revision": func(p *domain.PVCCopyPlan) {
			p.TemplateRevisionID = "another-revision"
			p.TemplateDigest = "sha256:" + strings.Repeat("e", 64)
		},
		"plan id":          func(p *domain.PVCCopyPlan) { p.PlanID = "another-plan" },
		"item id":          func(p *domain.PVCCopyPlan) { p.Items[0].ID = "another-item" },
		"execution bounds": func(p *domain.PVCCopyPlan) { p.MaxBytes *= 2; p.StorageQuotaBytes *= 2; p.TimeoutSeconds *= 2 },
		"unselected namespace": func(p *domain.PVCCopyPlan) {
			p.AllowedSourceNamespaces = append(p.AllowedSourceNamespaces, "unselected")
		},
		"all feature bindings": func(p *domain.PVCCopyPlan) {
			p.EnvironmentID, p.TargetNamespace, p.TemplateRevisionID, p.PlanID = "another-feature", "another-preview", "another-revision", "another-plan"
			p.TemplateDigest = "sha256:" + strings.Repeat("e", 64)
			p.Items[0].TargetName, p.Items[1].TargetName = "another-target-a", "another-target-b"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := fenceTestPlan(t)
			mutate(&p)
			p = fenceResealTestPlan(t, p)
			if p.Digest == base.Digest {
				t.Fatal("test did not change feature plan digest")
			}
			policyName, bindingName, err := FenceNames(p, "runner-ns", "runtime-runner")
			if err != nil || policyName != basePolicyName || bindingName != baseBindingName {
				t.Fatalf("feature plan required new fence/RBAC names: %s %s %v", policyName, bindingName, err)
			}
			gotPolicy, gotBinding, err := FenceManifests(p, "runner-ns", "runtime-runner")
			wantPolicy, wantBinding, _ := FenceManifests(base, "runner-ns", "runtime-runner")
			if err != nil || !reflect.DeepEqual(gotPolicy, wantPolicy) || !reflect.DeepEqual(gotBinding, wantBinding) {
				t.Fatal("feature change altered source fence specification, annotation or denial message")
			}
			f := &fenceTestCommands{t: t, policy: policy, binding: binding}
			if err := (&Kubectl{Commands: f}).RequireAdmissionFence(context.Background(), p, "runner-ns", "runtime-runner"); err != nil || f.probes != 3*len(p.Items) {
				t.Fatalf("previously provisioned source fence not reusable/enforcement not reprobed: %v", err)
			}
		})
	}
}

func TestFenceChangedSourceAuthorityRejectsStaleFence(t *testing.T) {
	base := fenceTestPlan(t)
	policy, binding := fenceTestObjects(t, base)
	oldPolicyName, oldBindingName, err := FenceNames(base, "runner-ns", "runtime-runner")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*domain.PVCCopyPlan, *string, *string){
		"source UID": func(p *domain.PVCCopyPlan, ns, sa *string) { p.Items[0].SourceUID = "replacement-uid" },
		"source identity": func(p *domain.PVCCopyPlan, ns, sa *string) {
			p.Items[0].SourceIdentity = "sha256:" + strings.Repeat("e", 64)
		},
		"source name": func(p *domain.PVCCopyPlan, ns, sa *string) { p.Items[0].SourceName = "replacement-claim" },
		"source namespace": func(p *domain.PVCCopyPlan, ns, sa *string) {
			p.Items[0].SourceNamespace = "replacement-source"
			p.AllowedSourceNamespaces = append(p.AllowedSourceNamespaces, "replacement-source")
		},
		"source set": func(p *domain.PVCCopyPlan, ns, sa *string) { p.Items = p.Items[:1] },
		"tenant": func(p *domain.PVCCopyPlan, ns, sa *string) {
			p.TenantID = "another-tenant"
			for i := range p.Items {
				p.Items[i].SourceTenantID = p.TenantID
			}
		},
		"project":          func(p *domain.PVCCopyPlan, ns, sa *string) { p.ProjectID = "another-project" },
		"runner namespace": func(p *domain.PVCCopyPlan, ns, sa *string) { *ns = "another-runner-ns" },
		"runner SA":        func(p *domain.PVCCopyPlan, ns, sa *string) { *sa = "another-runtime-runner" },
		"image": func(p *domain.PVCCopyPlan, ns, sa *string) {
			p.Image = "example.test/runner@sha256:" + strings.Repeat("e", 64)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			p := fenceTestPlan(t)
			ns, sa := "runner-ns", "runtime-runner"
			mutate(&p, &ns, &sa)
			p = fenceResealTestPlan(t, p)
			policyName, bindingName, err := FenceNames(p, ns, sa)
			if err != nil || policyName == oldPolicyName || bindingName == oldBindingName {
				t.Fatalf("changed source authority reused old reader identity: %v", err)
			}
			f := &fenceTestCommands{t: t, policy: policy, binding: binding}
			if err := (&Kubectl{Commands: f}).RequireAdmissionFence(context.Background(), p, ns, sa); !errors.Is(err, ErrUnsafe) || f.probes != 0 {
				t.Fatalf("stale source fence accepted/probed: %v", err)
			}
			// Renaming an old object cannot make its old digest/spec acceptable.
			stalePolicy, staleBinding := fenceTestObjects(t, base)
			stalePolicy["metadata"].(map[string]any)["name"] = policyName
			staleBinding["metadata"].(map[string]any)["name"] = bindingName
			f = &fenceTestCommands{t: t, policy: stalePolicy, binding: staleBinding}
			if err := (&Kubectl{Commands: f}).RequireAdmissionFence(context.Background(), p, ns, sa); !errors.Is(err, ErrUnsafe) || f.probes != 0 {
				t.Fatalf("renamed stale fence accepted/probed: %v", err)
			}
		})
	}
}

func TestFenceRenderingBeforeAnyFeatureExists(t *testing.T) {
	base := fenceTestPlan(t)
	sourceOnly := domain.PVCCopyPlan{TenantID: base.TenantID, ProjectID: base.ProjectID, Image: base.Image}
	for _, item := range base.Items {
		sourceOnly.Items = append(sourceOnly.Items, domain.PVCCopyItem{SourceTenantID: item.SourceTenantID, SourceNamespace: item.SourceNamespace, SourceName: item.SourceName, SourceUID: item.SourceUID, SourceIdentity: item.SourceIdentity})
	}
	policyName, bindingName, err := FenceNames(sourceOnly, "runner-ns", "runtime-runner")
	wantPolicyName, wantBindingName, _ := FenceNames(base, "runner-ns", "runtime-runner")
	if err != nil || policyName != wantPolicyName || bindingName != wantBindingName {
		t.Fatalf("pre-feature reader names differ: %v", err)
	}
	policy, binding, err := FenceManifests(sourceOnly, "runner-ns", "runtime-runner")
	wantPolicy, wantBinding, _ := FenceManifests(base, "runner-ns", "runtime-runner")
	if err != nil || !reflect.DeepEqual(policy, wantPolicy) || !reflect.DeepEqual(binding, wantBinding) {
		t.Fatal("pre-feature source policy/binding differs from feature policy/binding")
	}
	// Rendering metadata is not execution authority. Refuse incomplete plans
	// before any metadata/probe transport or other action can occur.
	if err := (&Kubectl{}).RequireAdmissionFence(context.Background(), sourceOnly, "runner-ns", "runtime-runner"); !errors.Is(err, ErrUnsafe) {
		t.Fatal("source-only rendering input was accepted as an execution plan")
	}
}

func TestFenceRenderingRejectsInvalidSourceAuthority(t *testing.T) {
	for name, mutate := range map[string]func(*domain.PVCCopyPlan){
		"missing sources":     func(p *domain.PVCCopyPlan) { p.Items = nil },
		"missing tenant":      func(p *domain.PVCCopyPlan) { p.TenantID = "" },
		"missing project":     func(p *domain.PVCCopyPlan) { p.ProjectID = "" },
		"cross tenant":        func(p *domain.PVCCopyPlan) { p.Items[0].SourceTenantID = "other" },
		"duplicate source":    func(p *domain.PVCCopyPlan) { p.Items = append(p.Items, p.Items[0]) },
		"namespace injection": func(p *domain.PVCCopyPlan) { p.Items[0].SourceNamespace = "bad' || true" },
		"claim injection":     func(p *domain.PVCCopyPlan) { p.Items[0].SourceName = "bad' || true" },
		"invalid claim":       func(p *domain.PVCCopyPlan) { p.Items[0].SourceName = "a..b" },
		"missing UID":         func(p *domain.PVCCopyPlan) { p.Items[0].SourceUID = "" },
		"UID injection":       func(p *domain.PVCCopyPlan) { p.Items[0].SourceUID = "bad/name" },
		"source identity":     func(p *domain.PVCCopyPlan) { p.Items[0].SourceIdentity = "not-a-digest" },
		"mutable image":       func(p *domain.PVCCopyPlan) { p.Image = "example.test/runner:latest" },
	} {
		t.Run(name, func(t *testing.T) {
			p := fenceTestPlan(t)
			mutate(&p)
			if _, _, err := FenceNames(p, "runner-ns", "runtime-runner"); !errors.Is(err, ErrUnsafe) {
				t.Fatal("unsafe source authority accepted by reader renderer")
			}
			if _, _, err := FenceManifests(p, "runner-ns", "runtime-runner"); !errors.Is(err, ErrUnsafe) {
				t.Fatal("unsafe source authority accepted by policy renderer")
			}
		})
	}
}

func TestFencePreSchedulingSecurityGuardsAndWeakenedPolicyDrift(t *testing.T) {
	// Each clause must appear in BOTH source-consumer exceptions and every
	// Runner/source-namespace CREATE exception, not only in a documentation
	// annotation or the post-create worker readback.
	guards := []string{
		"has(object.spec.securityContext)",
		"!has(object.spec.securityContext.fsGroup)",
		"!has(object.spec.securityContext.fsGroupChangePolicy)",
		"!has(object.spec.securityContext.supplementalGroups)",
		"!has(object.spec.securityContext.supplementalGroupsPolicy)",
		"!has(object.spec.securityContext.seLinuxOptions)",
		"!has(object.spec.securityContext.seLinuxChangePolicy)",
		"!has(object.spec.securityContext.sysctls)",
		"!has(object.spec.securityContext.windowsOptions)",
		"!has(object.spec.securityContext.appArmorProfile)",
		"object.spec.securityContext.seccompProfile.type == 'RuntimeDefault'",
		"!has(object.spec.securityContext.seccompProfile.localhostProfile)",
		"(object.spec.securityContext.runAsUser == 0 && object.spec.securityContext.runAsGroup == 0 && object.spec.securityContext.runAsNonRoot == false)",
		"(object.spec.securityContext.runAsUser == 10001 && object.spec.securityContext.runAsGroup == 10001 && object.spec.securityContext.runAsNonRoot == true)",
		"!has(object.spec.containers[0].securityContext.seLinuxOptions)",
		"!has(object.spec.containers[0].securityContext.runAsUser)",
		"!has(object.spec.containers[0].securityContext.runAsGroup)",
		"!has(object.spec.containers[0].securityContext.runAsNonRoot)",
		"!has(object.spec.containers[0].securityContext.seccompProfile)",
		"!has(object.spec.containers[0].securityContext.appArmorProfile)",
		"!has(object.spec.containers[0].securityContext.procMount)",
		"!has(object.spec.containers[0].securityContext.windowsOptions)",
		"object.spec.containers[0].securityContext.capabilities.drop == ['ALL']",
		"object.spec.containers[0].securityContext.capabilities.add == ['DAC_OVERRIDE']",
		"size(object.spec.containers[0].securityContext.capabilities.add) == 0",
		"k != 'seccomp.security.alpha.kubernetes.io/pod'",
		"!k.startsWith('container.seccomp.security.alpha.kubernetes.io/')",
		"!k.startsWith('container.apparmor.security.beta.kubernetes.io/')",
		"object.spec.activeDeadlineSeconds >= 31",
		"object.spec.activeDeadlineSeconds <= 21630",
		"object.spec.terminationGracePeriodSeconds == 5",
		"object.spec.containers[0].command == ['sleep']",
		"size(object.spec.containers[0].args) == 1",
		"object.spec.containers[0].args[0].matches('^[0-9]{1,6}$')",
		"int(object.spec.containers[0].args[0]) == object.spec.activeDeadlineSeconds",
		"!has(object.spec.containers[0].workingDir)",
		"object.spec.containers[0].terminationMessagePath == '/dev/termination-log'",
		"!has(object.spec.containers[0].lifecycle)",
		"!has(object.spec.containers[0].livenessProbe)",
		"!has(object.spec.containers[0].readinessProbe)",
		"!has(object.spec.containers[0].startupProbe)",
	}
	for _, guard := range guards {
		t.Run(guard, func(t *testing.T) {
			p := fenceTestPlan(t)
			policy, binding := fenceTestObjects(t, p)
			for _, value := range policy["spec"].(map[string]any)["validations"].([]any) {
				validation := value.(map[string]any)
				expression := validation["expression"].(string)
				if !strings.Contains(expression, guard) {
					t.Fatalf("missing admission-time guard %q", guard)
				}
				validation["expression"] = strings.ReplaceAll(expression, guard, "true")
			}
			// Same source-bound names, annotation and accepted generation cannot
			// certify the weaker policy; refuse before any probe/create request.
			f := &fenceTestCommands{t: t, policy: policy, binding: binding}
			if err := (&Kubectl{Commands: f}).RequireAdmissionFence(context.Background(), p, "runner-ns", "runtime-runner"); !errors.Is(err, ErrUnsafe) || f.probes != 0 {
				t.Fatalf("weakened metadata/security fence accepted: %v", err)
			}
		})
	}
}

func TestFenceInjectionDenialProofIsRequiredBeforeScheduling(t *testing.T) {
	for _, injection := range []string{"fsGroup", "seLinuxOptions"} {
		for _, mode := range []string{"exact fence denial", "admitted", "unrelated denial"} {
			t.Run(injection+"/"+mode, func(t *testing.T) {
				p := fenceTestPlan(t)
				policy, binding := fenceTestObjects(t, p)
				f := &fenceTestCommands{t: t, policy: policy, binding: binding}
				injectionsSeen := 0
				f.probe = func(_ context.Context, args []string, pod map[string]any, out io.Writer) error {
					spec := pod["spec"].(map[string]any)
					security := spec["securityContext"].(map[string]any)
					volume := spec["volumes"].([]any)[0].(map[string]any)["persistentVolumeClaim"].(map[string]any)
					if security[injection] != nil {
						injectionsSeen++
						if volume["readOnly"] != true || security["runAsUser"] != float64(10001) || security["runAsGroup"] != float64(10001) || security["runAsNonRoot"] != true || security["seccompProfile"].(map[string]any)["type"] != "RuntimeDefault" {
							t.Fatal("metadata proof must target an otherwise allowed read-only helper")
						}
						found := false
						for _, item := range p.Items {
							if item.SourceNamespace == args[5] && item.SourceName == volume["claimName"] {
								found = true
								if pod["metadata"].(map[string]any)["name"] != SourceHelperName(PVCRef{Namespace: item.SourceNamespace, Name: item.SourceName, UID: item.SourceUID}) {
									t.Fatal("injection proof did not use the exact authorized source helper name")
								}
							}
						}
						if !found {
							t.Fatal("injection proof references an unselected source")
						}
						if mode == "admitted" {
							return nil
						}
						if mode == "unrelated denial" {
							_, _ = io.WriteString(out, "PodSecurity forbidden: private metadata")
							return errors.New("private stderr")
						}
					}
					name := policy["metadata"].(map[string]any)["name"].(string)
					digest := policy["metadata"].(map[string]any)["annotations"].(map[string]any)[fenceDigestAnnotation].(string)
					_, _ = io.WriteString(out, "ValidatingAdmissionPolicy '"+name+"' with binding '"+name+"' denied request: "+fenceMessage(digest))
					return errors.New("exit status 1")
				}
				err := (&Kubectl{Commands: f}).RequireAdmissionFence(context.Background(), p, "runner-ns", "runtime-runner")
				if mode == "exact fence denial" {
					if err != nil || injectionsSeen != len(p.Items) || f.probes != 3*len(p.Items) {
						t.Fatalf("missing metadata denial evidence: %v", err)
					}
				} else if !errors.Is(err, ErrUnsafe) || injectionsSeen != 1 || strings.Contains(err.Error(), "private") {
					t.Fatalf("missing/unrelated metadata denial falsely proved fence: %v", err)
				}
			})
		}
	}
}
