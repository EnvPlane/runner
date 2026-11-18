package orchestrator

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/envplane/contracts/domain"
)

type platformHelmFixture struct {
	HelmExecutor
	events       []string
	live         map[string]map[string]any
	chart        []map[string]any
	denyWrite    string
	missingAfter string
	owned        bool
}

func (f *platformHelmFixture) TemplateServices(_ context.Context, _ HelmUpgradeOptions) ([]map[string]any, error) {
	return f.chart, nil
}

func (f *platformHelmFixture) IsNamespaceManaged(_ context.Context, _, _, _ string) (bool, error) {
	return f.owned, nil
}

func (f *platformHelmFixture) UpgradeInstall(_ context.Context, opts HelmUpgradeOptions) error {
	f.events = append(f.events, "helm")
	f.live["Service/chart-web"] = fixtureService("chart-web", opts.Namespace, opts.ReleaseName)
	return nil
}

func (f *platformHelmFixture) PutPlatformResource(_ context.Context, r domain.RenderedResource, plan string) (map[string]any, error) {
	f.events = append(f.events, "put:"+r.Kind)
	if r.Kind == f.denyWrite {
		return nil, errors.New("API write denied")
	}
	meta := map[string]any{"name": r.Name, "namespace": r.Namespace, "annotations": map[string]any{helmPlatformPlanOwner: plan}}
	f.live[r.Kind+"/"+r.Name] = map[string]any{"apiVersion": helmPlatformAPIVersions[r.Kind], "kind": r.Kind, "metadata": meta, "spec": r.Manifest["spec"]}
	return platformMap(r.Manifest, "spec"), nil
}

func (f *platformHelmFixture) GetPlatformResource(_ context.Context, kind, _, name string) (map[string]any, error) {
	f.events = append(f.events, "get:"+kind)
	if kind == f.missingAfter {
		return nil, nil
	}
	return f.live[kind+"/"+name], nil
}

func fixtureService(name, namespace, release string) map[string]any {
	return map[string]any{
		"apiVersion": "v1", "kind": "Service",
		"metadata": map[string]any{"name": name, "namespace": namespace, "annotations": map[string]any{
			"meta.helm.sh/release-name": release, "meta.helm.sh/release-namespace": namespace}},
		"spec": map[string]any{"selector": map[string]any{"app": "fixture"}, "ports": []any{
			map[string]any{"port": 80, "name": "http"}}},
	}
}

func platformProjectFixture() (domain.Environment, domain.ProjectConfig) {
	env := domain.Environment{ID: "feature", Project: "checkout", Namespace: "envplane-pr-feature",
		Source: domain.SCMSource{PullRequestID: "42"}}
	var templates []compiledManifestTemplate
	for _, kind := range []string{"LimitRange", "ResourceQuota", "NetworkPolicy", "Ingress", "Deployment"} {
		spec := "spec: {}"
		if kind == "Ingress" {
			spec = "spec: {defaultBackend: {service: {name: base-web, port: {number: 80}}}}"
		}
		templates = append(templates, compiledManifestTemplate{
			Kind: kind, Namespace: "envplane-pr-{{ .PRNumber }}",
			YAML: "apiVersion: " + helmPlatformAPIVersions[kind] + "\nkind: " + kind + "\nmetadata: {name: sample, namespace: 'envplane-pr-{{ .PRNumber }}'}\n" + spec + "\n",
		})
	}
	config := domain.ProjectConfig{Config: map[string]any{
		"bootstrapSessionData": map[string]any{"manifestTemplates": templates},
	}}
	return env, config
}

func platformRenderedPlan(t *testing.T, b *HelmDirectBackend, env domain.Environment, config domain.ProjectConfig) *domain.EnvironmentReleasePlan {
	t.Helper()
	manifests, err := b.Render(context.Background(), env, config)
	if err != nil || len(manifests) != 5 {
		t.Fatalf("expected one Helm descriptor and four managed resources, got %d: %v", len(manifests), err)
	}
	plan := &domain.EnvironmentReleasePlan{PlanID: "release-plan"}
	for _, item := range manifests[1:] {
		object, err := strictPlatformManifest(string(item.Content))
		if err != nil {
			t.Fatal(err)
		}
		metadata := platformMap(object, "metadata")
		if got := platformString(metadata, "namespace"); got != env.Namespace {
			t.Fatalf("compiled namespace = %q, want %q", got, env.Namespace)
		}
		plan.RenderedResources = append(plan.RenderedResources, domain.RenderedResource{
			ResourceID: item.Kind + "/" + env.Namespace + "/sample", Kind: item.Kind,
			Name: "sample", Namespace: env.Namespace, Manifest: object, Digest: "signed-digest",
		})
		if item.Kind == "Ingress" && !strings.Contains(string(item.Content), "chart-web") {
			t.Fatal("compiled Ingress did not bind to the single chart Service")
		}
	}
	return plan
}

func TestHelmPlatformResourcesApplyInOrderAndVerifyReadback(t *testing.T) {
	env, config := platformProjectFixture()
	f := &platformHelmFixture{chart: []map[string]any{fixtureService("chart-web", env.Namespace, "")}, owned: true, live: map[string]map[string]any{}}
	b := NewHelmDirectBackendWithExecutor(nil, f)
	plan := platformRenderedPlan(t, b, env, config)
	want := []string{"put:LimitRange", "put:ResourceQuota", "put:NetworkPolicy", "helm", "get:Service",
		"put:Ingress", "get:LimitRange", "get:ResourceQuota", "get:NetworkPolicy", "get:Ingress"}
	for _, operation := range []string{"create", "recreate"} {
		f.events = nil
		if err := b.ApplySignedPlatformRelease(t.Context(), env, config, plan); err != nil || !slices.Equal(want, f.events) {
			t.Fatalf("%s order=%v err=%v", operation, f.events, err)
		}
	}
	for _, item := range HelmPlatformReadbackReceipt(plan) {
		if item.Manifest != nil || item.Digest != "signed-digest" {
			t.Fatal("receipt must carry the signed reference but not the object")
		}
	}
	f.missingAfter = "NetworkPolicy"
	if err := b.VerifySignedPlatformRelease(t.Context(), env, config, plan); err == nil {
		t.Fatal("status must not report Ready with a missing policy")
	}
}

func TestHelmPlatformResourcesRefuseMissingUnownedAndAmbiguousInputs(t *testing.T) {
	env, config := platformProjectFixture()
	f := &platformHelmFixture{chart: []map[string]any{fixtureService("chart-web", env.Namespace, "")}, live: map[string]map[string]any{}}
	b := NewHelmDirectBackendWithExecutor(nil, f)
	plan := platformRenderedPlan(t, b, env, config)
	if err := b.ApplySignedPlatformRelease(t.Context(), env, config, plan); err == nil || len(f.events) != 0 {
		t.Fatal("unowned namespace was written")
	}
	f.owned = true
	if err := b.ApplySignedPlatformRelease(t.Context(), env, config, &domain.EnvironmentReleasePlan{}); err == nil || len(f.events) != 0 {
		t.Fatal("legacy plan omitted its required policies")
	}
	f.denyWrite = "NetworkPolicy"
	if err := b.ApplySignedPlatformRelease(t.Context(), env, config, plan); err == nil || slices.Contains(f.events, "helm") {
		t.Fatal("workload was deployed when NetworkPolicy was denied")
	}
	f.chart = append(f.chart, fixtureService("another-web", env.Namespace, ""))
	if _, err := b.Render(t.Context(), env, config); err == nil {
		t.Fatal("ambiguous Ingress Service rebinding was permitted")
	}
	env.Namespace = "foreign"
	if _, err := b.Render(t.Context(), env, config); err == nil {
		t.Fatal("compiled resources rendered outside owned environment scope")
	}
}
