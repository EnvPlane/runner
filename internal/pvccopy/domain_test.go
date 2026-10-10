package pvccopy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/envplane/contracts/domain"
)

func testDomain(t *testing.T) (DomainExecutor, domain.PVCCopyPlan, *fakeCluster) {
	t.Helper()
	leaf := testPlan()
	f := newFake(t, leaf)
	delete(f.pvcs, leaf.Target.Namespace+"/"+leaf.Target.Name)
	s := domain.PVCCopySource{TenantID: leaf.Tenant, Namespace: leaf.Source.Namespace, Name: leaf.Source.Name, UID: leaf.Source.UID, StorageClass: "local-path", AccessModes: []string{"ReadWriteOnce"}, RequestedBytes: 1 << 20, VolumeMode: "Filesystem", EnvironmentClass: "development"}
	p := domain.PVCCopyPlan{PlanID: "copy-plan", TenantID: leaf.Tenant, ProjectID: leaf.Project, EnvironmentID: leaf.Environment, TemplateRevisionID: "rev-1", TemplateDigest: "sha256:" + strings.Repeat("b", 64), TargetNamespace: leaf.Target.Namespace, AllowedSourceNamespaces: leaf.AllowedSourceNamespaces, Image: leaf.Image, MaxBytes: 1 << 20, TimeoutSeconds: 60, StorageQuotaBytes: 1 << 20, Items: []domain.PVCCopyItem{{ID: "data", SourceTenantID: s.TenantID, SourceNamespace: s.Namespace, SourceName: s.Name, SourceUID: s.UID, SourceRequestedBytes: s.RequestedBytes, TargetName: leaf.Target.Name, StorageClass: s.StorageClass, AccessModes: s.AccessModes, RequestedBytes: 1 << 20, Method: domain.PVCCopyFilesystemOffline, MaxBytes: 1 << 20, TimeoutSeconds: 60}}}
	permission := domain.PVCCopyPermission{PlanID: p.PlanID, TenantID: p.TenantID, ProjectID: p.ProjectID, EnvironmentID: p.EnvironmentID, TemplateRevisionID: p.TemplateRevisionID, TemplateDigest: p.TemplateDigest, TargetNamespace: p.TargetNamespace, AllowedSourceNamespaces: p.AllowedSourceNamespaces, AllowedImages: []string{p.Image}, MaxBytes: p.MaxBytes, TimeoutSeconds: p.TimeoutSeconds, StorageQuotaBytes: p.StorageQuotaBytes}
	var err error
	p, err = domain.CompilePVCCopyPlan(p, []domain.PVCCopySource{s}, permission)
	if err != nil {
		t.Fatal(err)
	}
	d := DomainExecutor{Kubectl: &Kubectl{Commands: f, RunnerImage: p.Image, ServiceAccount: "runner", AllowRootHelpers: true}, Permission: permission, Sources: []domain.PVCCopySource{s}, Authorize: func(context.Context, domain.PVCCopyPlan, []domain.PVCCopySource) error { return nil }}
	return d, p, f
}

func TestDomainNativePVCWFFCAndDurableRetry(t *testing.T) {
	d, p, f := testDomain(t)
	markers, err := d.ExecuteDomain(context.Background(), p, []string{"additional-trusted-ns", p.Items[0].SourceNamespace}, p.Image)
	if err != nil {
		t.Fatal(err)
	}
	if len(markers) != 1 || markers[0].TargetUID != "new-target-uid" || markers[0].ImmutablePlanDigest != p.Digest {
		t.Fatalf("marker: %+v", markers)
	}
	if f.createdPVCs != 1 || f.deletedPods != 2 {
		t.Fatalf("native lifecycle: %d %d", f.createdPVCs, f.deletedPods)
	}
	obj := f.pvcs[p.TargetNamespace+"/"+p.Items[0].TargetName]
	spec := obj["spec"].(map[string]any)
	if spec["dataSource"] != nil || spec["dataSourceRef"] != nil {
		t.Fatal("native target contains binding")
	}
	labels := metadata(obj)["labels"].(map[string]any)
	if labels["envplane.io/project"] != p.ProjectID || labels["envplane.io/environment-id"] != p.EnvironmentID || labels["envplane.io/pvc-copy-project"] != labelIdentity(p.ProjectID) {
		t.Fatal("lifecycle/copy labels collide")
	}
	// Simulate Flux reapplying its plain standard labels; copy ownership survives.
	labels["envplane.io/project"] = "plain-lifecycle-project"
	retry, err := d.ExecuteDomain(context.Background(), p, p.AllowedSourceNamespaces, p.Image)
	if err != nil || len(retry) != 1 || retry[0] != markers[0] || f.createdPVCs != 1 {
		t.Fatalf("retry: %v", err)
	}
	// A same-name PVC replacement cannot reuse the old completion marker.
	metadata(obj)["uid"] = "replacement-uid"
	if _, err := d.ExecuteDomain(context.Background(), p, p.AllowedSourceNamespaces, p.Image); err == nil {
		t.Fatal("replacement target accepted")
	}
}

func TestDomainAuthorityRefusalBeforeAnyTargetWrite(t *testing.T) {
	for _, kind := range []string{"uid", "storage-spec", "class-drift", "class-unknown", "annotation", "active-readonly", "active-controller", "production-approval", "tenant", "template", "trusted-image", "trusted-namespaces", "foreign-target", "authority", "missing-authority", "podsecurity", "target-consumer"} {
		t.Run(kind, func(t *testing.T) {
			d, p, f := testDomain(t)
			item := p.Items[0]
			source := f.pvcs[item.SourceNamespace+"/"+item.SourceName]
			namespaces := p.AllowedSourceNamespaces
			image := p.Image
			switch kind {
			case "uid":
				metadata(source)["uid"] = "new-source"
			case "storage-spec":
				source["spec"].(map[string]any)["storageClassName"] = "different"
			case "class-drift":
				metadata(source)["labels"].(map[string]any)["envplane.io/environment-class"] = "production"
			case "class-unknown":
				metadata(source)["labels"] = map[string]any{}
			case "annotation":
				metadata(source)["annotations"] = map[string]any{}
			case "active-readonly":
				f.pods[item.SourceNamespace+"/database"] = consumer(PVCRef{item.SourceNamespace, item.SourceName, item.SourceUID}, "Running", true)
			case "active-controller":
				f.controllers = []map[string]any{controller(PVCRef{item.SourceNamespace, item.SourceName, item.SourceUID}, "Deployment", false)}
			case "production-approval":
				d.Sources[0].EnvironmentClass = "production"
				metadata(source)["labels"].(map[string]any)["envplane.io/environment-class"] = "production"
			case "tenant":
				d.Sources[0].TenantID = "other-tenant"
			case "template":
				d.Permission.TemplateRevisionID = "other-revision"
			case "trusted-image":
				image = "registry.example/runner@sha256:" + strings.Repeat("f", 64)
			case "trusted-namespaces":
				namespaces = []string{"other"}
			case "foreign-target":
				f.pvcs[p.TargetNamespace+"/"+item.TargetName] = objectPVC(PVCRef{p.TargetNamespace, item.TargetName, "foreign"}, "foreign-pv")
			case "authority":
				d.Authorize = func(context.Context, domain.PVCCopyPlan, []domain.PVCCopySource) error {
					return errors.New("namespace is not owned or backend is not Flux")
				}
			case "missing-authority":
				d.Authorize = nil
			case "podsecurity":
				f.failPods = true
			case "target-consumer":
				f.pods[p.TargetNamespace+"/database"] = consumer(PVCRef{p.TargetNamespace, item.TargetName, ""}, "Pending", false)
			}
			if _, err := d.ExecuteDomain(context.Background(), p, namespaces, image); err == nil {
				t.Fatal("unsafe domain copy succeeded")
			}
			if f.createdPVCs != 0 || len(f.createdPods) != 0 {
				t.Fatalf("refusal after writes: PVCs %d Pods %d", f.createdPVCs, len(f.createdPods))
			}
			if _, err := os.Stat(filepath.Join(f.target, StateDirectory)); !os.IsNotExist(err) {
				t.Fatalf("target was claimed: %v", err)
			}
		})
	}
}

func TestQuantityBounds(t *testing.T) {
	for input, want := range map[string]int64{"1Mi": 1 << 20, "1.5Gi": 3 << 29, "123": 123, "1G": 1000000000, "0.1": 1} {
		got, err := quantityBytes(input)
		if err != nil || got != want {
			t.Fatalf("%s: %d %v", input, got, err)
		}
	}
	for _, input := range []string{"", "-1Gi", "0", "99999999999999999999999Ti", "1e3", "bogus"} {
		if _, err := quantityBytes(input); err == nil {
			t.Fatalf("unsafe quantity %q", input)
		}
	}
}
