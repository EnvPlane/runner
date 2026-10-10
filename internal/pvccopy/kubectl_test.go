package pvccopy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeCluster implements kubectl JSON and argv semantics, but exec runs the
// actual helper against real temporary files. No live cluster is contacted.
type fakeCluster struct {
	mu                                                                                sync.Mutex
	pvcs                                                                              map[string]map[string]any
	pods                                                                              map[string]map[string]any
	controllers                                                                       []map[string]any
	source, target                                                                    string
	createdPods                                                                       []map[string]any
	createdPVCs, deletedPods, exports                                                 int
	failPods, denyControllers, blockExport, wrongReceipt, changeSource, replaceHelper bool
	mutateHelper                                                                      string
}

func valueArg(args []string, flag string) string {
	i := slices.Index(args, flag)
	if i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}
func cloneMap(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	var n map[string]any
	_ = json.Unmarshal(b, &n)
	return n
}
func metadata(m map[string]any) map[string]any { return m["metadata"].(map[string]any) }
func objectPVC(ref PVCRef, pv string) map[string]any {
	return map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": map[string]any{"namespace": ref.Namespace, "name": ref.Name, "uid": ref.UID, "annotations": map[string]any{"envplane.io/pvc-copy-offline": "true"}, "labels": map[string]any{"envplane.io/environment-class": "development"}}, "spec": map[string]any{"volumeName": pv, "volumeMode": "Filesystem", "storageClassName": "local-path", "accessModes": []string{"ReadWriteOnce"}, "resources": map[string]any{"requests": map[string]string{"storage": "1Mi"}}}, "status": map[string]any{"phase": "Bound"}}
}
func newFake(t *testing.T, p Plan) *fakeCluster {
	t.Helper()
	f := &fakeCluster{pvcs: map[string]map[string]any{}, pods: map[string]map[string]any{}, source: t.TempDir(), target: t.TempDir()}
	f.pvcs[p.Source.Namespace+"/"+p.Source.Name] = objectPVC(p.Source, "source-pv")
	f.pvcs[p.Target.Namespace+"/"+p.Target.Name] = objectPVC(p.Target, "target-pv")
	writeTestFile(t, f.source, "dir/file", []byte("actual filesystem data\x00\xff"))
	return f
}

func (f *fakeCluster) Run(ctx context.Context, args []string, input io.Reader, output io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ns := valueArg(args, "-n")
	if args[0] == "exec" {
		i := slices.Index(args, "--")
		if i < 0 {
			return fmt.Errorf("missing exec boundary")
		}
		name := args[slices.Index(args, "-n")+2]
		f.mu.Lock()
		pod := f.pods[ns+"/"+name]
		if pod == nil {
			f.mu.Unlock()
			return errors.New("missing pod")
		}
		vol := pod["spec"].(map[string]any)["volumes"].([]any)[0].(map[string]any)["persistentVolumeClaim"].(map[string]any)
		root := f.target
		if vol["readOnly"] == true {
			root = f.source
		}
		block, wrong, change := f.blockExport, f.wrongReceipt, f.changeSource
		command := args[i+2]
		if command == "pvc-export" {
			f.exports++
		}
		exportCount := f.exports
		f.mu.Unlock()
		if block && command == "pvc-export" {
			<-ctx.Done()
			return ctx.Err()
		}
		if wrong && command == "pvc-import" {
			var b bytes.Buffer
			if err := HelperCLI(ctx, args[i+2:], root, input, &b); err != nil {
				return err
			}
			var r Receipt
			if err := json.Unmarshal(b.Bytes(), &r); err != nil {
				return err
			}
			r.Bytes++
			return json.NewEncoder(output).Encode(r)
		}
		err := HelperCLI(ctx, args[i+2:], root, input, output)
		if err == nil && change && command == "pvc-export" && exportCount == 1 {
			err = os.WriteFile(filepath.Join(root, "dir/file"), []byte("changed source bytes"), 0600)
		}
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	respond := func(v any) error { return json.NewEncoder(output).Encode(v) }
	switch args[0] {
	case "get":
		switch args[1] {
		case "pvc":
			obj := f.pvcs[ns+"/"+args[2]]
			if obj == nil {
				if slices.Contains(args, "--ignore-not-found=true") {
					return nil
				}
				return errors.New("missing PVC")
			}
			return respond(obj)
		case "pod":
			obj := f.pods[ns+"/"+args[2]]
			if obj == nil {
				return errors.New("missing Pod")
			}
			copy := cloneMap(obj)
			if f.replaceHelper {
				metadata(copy)["uid"] = "replacement"
			}
			return respond(copy)
		case "pods":
			var list []any
			for _, obj := range f.pods {
				if metadata(obj)["namespace"] == ns {
					list = append(list, obj)
				}
			}
			return respond(map[string]any{"items": list})
		default:
			if f.denyControllers {
				return errors.New("forbidden controller list")
			}
			return respond(map[string]any{"items": f.controllers})
		}
	case "create":
		var obj map[string]any
		if err := json.NewDecoder(input).Decode(&obj); err != nil {
			return err
		}
		if obj["kind"] == "Pod" {
			if f.failPods {
				return errors.New("PodSecurity admission denied")
			}
			if slices.Contains(args, "--dry-run=server") {
				return respond(obj)
			}
			spec := obj["spec"].(map[string]any)
			container := spec["containers"].([]any)[0].(map[string]any)
			switch f.mutateHelper {
			case "fsGroup":
				spec["securityContext"].(map[string]any)["fsGroup"] = float64(10001)
			case "selinux":
				spec["securityContext"].(map[string]any)["seLinuxOptions"] = map[string]any{"level": "s0"}
			case "capabilities":
				container["securityContext"].(map[string]any)["capabilities"] = map[string]any{"drop": []string{"ALL"}, "add": []string{"SYS_ADMIN"}}
			case "privilege":
				container["securityContext"].(map[string]any)["allowPrivilegeEscalation"] = true
			case "root":
				spec["securityContext"].(map[string]any)["runAsUser"] = float64(0)
			case "hostPID":
				spec["hostPID"] = true
			case "image":
				container["image"] = "other:latest"
			case "token":
				spec["automountServiceAccountToken"] = true
			case "writable":
				container["volumeMounts"].([]any)[0].(map[string]any)["readOnly"] = false
			case "sidecar":
				spec["containers"] = append(spec["containers"].([]any), map[string]any{"name": "sidecar", "image": "other"})
			case "deadline":
				spec["activeDeadlineSeconds"] = float64(0)
			}
			key := ns + "/" + metadata(obj)["name"].(string)
			if f.pods[key] != nil {
				return errors.New("already exists")
			}
			metadata(obj)["uid"] = fmt.Sprintf("helper-uid-%d", len(f.createdPods))
			obj["status"] = map[string]any{"phase": "Running", "conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
			f.createdPods = append(f.createdPods, cloneMap(obj))
			f.pods[key] = obj
			vol := obj["spec"].(map[string]any)["volumes"].([]any)[0].(map[string]any)["persistentVolumeClaim"].(map[string]any)
			if vol["readOnly"] == false {
				target := f.pvcs[ns+"/"+vol["claimName"].(string)]
				target["status"] = map[string]any{"phase": "Bound"}
				target["spec"].(map[string]any)["volumeName"] = "target-pv"
			}
			return respond(obj)
		}
		if obj["kind"] != "PersistentVolumeClaim" {
			return errors.New("unexpected mutation")
		}
		key := ns + "/" + metadata(obj)["name"].(string)
		if f.pvcs[key] != nil {
			return errors.New("already exists")
		}
		metadata(obj)["uid"] = "new-target-uid"
		obj["status"] = map[string]any{"phase": "Pending"}
		f.pvcs[key] = obj
		f.createdPVCs++
		return respond(obj)
	case "delete":
		if !slices.Contains(args, "--raw") {
			return errors.New("unsafe non-UID deletion")
		}
		raw := valueArg(args, "--raw")
		parts := strings.Split(raw, "/")
		if len(parts) != 7 || parts[5] != "pods" {
			return errors.New("non-helper deletion")
		}
		var body struct {
			Preconditions struct {
				UID string `json:"uid"`
			} `json:"preconditions"`
		}
		if err := json.NewDecoder(input).Decode(&body); err != nil {
			return err
		}
		key := parts[4] + "/" + parts[6]
		obj := f.pods[key]
		if obj == nil || metadata(obj)["uid"] != body.Preconditions.UID {
			return errors.New("UID precondition failed")
		}
		delete(f.pods, key)
		f.deletedPods++
		return nil
	}
	return errors.New("unsupported kubectl operation")
}

func testExecutor(f *fakeCluster, p Plan) Executor {
	k := &Kubectl{Commands: f, RunnerImage: p.Image, ServiceAccount: "runner", AllowRootHelpers: true}
	return Executor{Transport: k, Policy: Policy{RunnerImage: p.Image, SourceNamespaces: []string{p.Source.Namespace}, Target: p.AllowedTarget}, Permission: func(context.Context, Plan, PVC, PVC) error { return nil }}
}

func TestKubectlExecutorRealBytesAndSafety(t *testing.T) {
	p := testPlan()
	f := newFake(t, p)
	e := testExecutor(f, p)
	m, err := e.Execute(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(f.target, "dir/file"))
	if err != nil || string(got) != "actual filesystem data\x00\xff" {
		t.Fatalf("payload: %v", err)
	}
	if m.TargetUID != p.Target.UID || f.exports != 2 || f.deletedPods != 2 || f.createdPVCs != 0 {
		t.Fatalf("bad lifecycle: %+v %+v", m, f)
	}
	for i, obj := range f.createdPods {
		name := metadata(obj)["name"].(string)
		if i == 0 && name != SourceHelperName(p.Source) {
			t.Fatal("source helper not resourceNames bound")
		}
		if i == 1 && !strings.HasPrefix(name, "pvccopy-target-") {
			t.Fatal("target helper lacks random namespace")
		}
		spec := obj["spec"].(map[string]any)
		if _, exists := spec["securityContext"].(map[string]any)["fsGroup"]; exists {
			t.Fatal("source-changing fsGroup")
		}
		if spec["automountServiceAccountToken"] != false || spec["serviceAccountName"] != "runner" {
			t.Fatal("helper identity escalation")
		}
		container := spec["containers"].([]any)[0].(map[string]any)
		if container["command"].([]any)[0] != "sleep" || container["image"] != p.Image {
			t.Fatal("helper command/image")
		}
		caps := container["securityContext"].(map[string]any)["capabilities"].(map[string]any)
		if caps["drop"].([]any)[0] != "ALL" {
			t.Fatal("missing cap drop")
		}
		mount := container["volumeMounts"].([]any)[0].(map[string]any)
		if i == 0 && (mount["readOnly"] != true || mount["recursiveReadOnly"] != "Enabled" || len(caps["add"].([]any)) != 1) {
			t.Fatal("source not strictly read-only")
		}
	}
	retry, err := e.Execute(context.Background(), p)
	if err != nil || retry != m {
		t.Fatalf("durable executor retry: %v", err)
	}
}

func consumer(ref PVCRef, phase string, readonly bool) map[string]any {
	return map[string]any{"metadata": map[string]any{"namespace": ref.Namespace, "name": "database", "uid": "consumer-uid"}, "spec": map[string]any{"volumes": []any{map[string]any{"persistentVolumeClaim": map[string]any{"claimName": ref.Name, "readOnly": readonly}}}}, "status": map[string]any{"phase": phase}}
}
func controller(ref PVCRef, kind string, safe bool) map[string]any {
	spec := map[string]any{"template": map[string]any{"spec": consumer(ref, "Running", false)["spec"]}}
	switch kind {
	case "Deployment", "StatefulSet", "ReplicaSet":
		spec["replicas"] = float64(1)
		if safe {
			spec["replicas"] = float64(0)
		}
	case "Job", "CronJob":
		spec["suspend"] = safe
	}
	return map[string]any{"kind": kind, "metadata": map[string]any{"namespace": ref.Namespace, "name": "db"}, "spec": spec, "status": map[string]any{}}
}

func TestExecutorAuthorityAndUnknownOutcomes(t *testing.T) {
	for _, kind := range []string{"readonly-consumer", "writer", "pending-consumer", "target-consumer", "uid", "same-pv", "block-mode", "deleting", "offline-annotation", "trusted-image", "trusted-namespace", "permission", "missing-permission", "controllers-forbidden", "active-controller", "podsecurity", "wrong-receipt", "source-changed", "timeout", "helper-replacement"} {
		t.Run(kind, func(t *testing.T) {
			p := testPlan()
			f := newFake(t, p)
			e := testExecutor(f, p)
			source := f.pvcs[p.Source.Namespace+"/"+p.Source.Name]
			switch kind {
			case "readonly-consumer", "writer", "pending-consumer":
				phase := "Running"
				if kind == "pending-consumer" {
					phase = "Pending"
				}
				f.pods[p.Source.Namespace+"/database"] = consumer(p.Source, phase, kind == "readonly-consumer")
			case "target-consumer":
				f.pods[p.Target.Namespace+"/database"] = consumer(p.Target, "Running", false)
			case "uid":
				metadata(source)["uid"] = "replacement"
			case "same-pv":
				source["spec"].(map[string]any)["volumeName"] = "target-pv"
			case "block-mode":
				source["spec"].(map[string]any)["volumeMode"] = "Block"
			case "deleting":
				metadata(source)["deletionTimestamp"] = "now"
			case "offline-annotation":
				metadata(source)["annotations"] = map[string]any{}
			case "trusted-image":
				e.Policy.RunnerImage = "runner:latest"
			case "trusted-namespace":
				e.Policy.SourceNamespaces = []string{"other"}
			case "permission":
				e.Permission = func(context.Context, Plan, PVC, PVC) error { return ErrUnsafe }
			case "missing-permission":
				e.Permission = nil
			case "controllers-forbidden":
				f.denyControllers = true
			case "active-controller":
				f.controllers = []map[string]any{controller(p.Source, "StatefulSet", false)}
			case "podsecurity":
				f.failPods = true
			case "wrong-receipt":
				f.wrongReceipt = true
			case "source-changed":
				f.changeSource = true
			case "timeout":
				p.Timeout = 30 * time.Millisecond
				f.blockExport = true
			case "helper-replacement":
				f.replaceHelper = true
			}
			if _, err := e.Execute(context.Background(), p); err == nil {
				t.Fatal("unsafe executor success")
			}
			if _, err := os.Stat(filepath.Join(f.target, StateDirectory, "complete.json")); !os.IsNotExist(err) {
				t.Fatalf("completion exists: %v", err)
			}
			if kind != "source-changed" {
				b, _ := os.ReadFile(filepath.Join(f.source, "dir/file"))
				if string(b) != "actual filesystem data\x00\xff" {
					t.Fatal("source changed")
				}
			}
		})
	}
}

func TestControllerFence(t *testing.T) {
	ref := testPlan().Source
	for _, kind := range []string{"Deployment", "StatefulSet", "ReplicaSet", "Job", "CronJob", "DaemonSet"} {
		if err := controllerSafe(controller(ref, kind, false), ref); err == nil {
			t.Fatalf("active %s allowed", kind)
		}
		err := controllerSafe(controller(ref, kind, true), ref)
		if kind != "DaemonSet" && err != nil {
			t.Fatalf("fenced %s refused: %v", kind, err)
		}
		if kind == "DaemonSet" && err == nil {
			t.Fatal("DaemonSet allowed")
		}
	}
	obj := controller(ref, "Deployment", true)
	obj["spec"].(map[string]any)["replicas"] = nil
	if controllerSafe(obj, ref) == nil {
		t.Fatal("unknown replicas allowed")
	}
}

func TestAdmissionMutationsRefusedAndKnownUIDCleaned(t *testing.T) {
	for _, kind := range []string{"fsGroup", "selinux", "capabilities", "privilege", "root", "hostPID", "image", "token", "writable", "sidecar", "deadline"} {
		t.Run(kind, func(t *testing.T) {
			p := testPlan()
			f := newFake(t, p)
			f.mutateHelper = kind
			e := testExecutor(f, p)
			if kind == "root" {
				e.Transport.(*Kubectl).AllowRootHelpers = false
			}
			if _, err := e.Execute(context.Background(), p); err == nil {
				t.Fatal("admitted mutation accepted")
			}
			if f.exports != 0 || f.deletedPods != 1 || len(f.pods) != 0 {
				t.Fatalf("unexpected exec/cleanup: exports %d deleted %d left %d", f.exports, f.deletedPods, len(f.pods))
			}
			if _, err := os.Stat(filepath.Join(f.target, StateDirectory)); !os.IsNotExist(err) {
				t.Fatal("admission error claimed target")
			}
		})
	}
}

func TestSourceHelperDeterministicAndOrphanNeverAdopted(t *testing.T) {
	p := testPlan()
	hash := sha256.Sum256([]byte(p.Source.Namespace + "/" + p.Source.Name + "/" + p.Source.UID))
	want := fmt.Sprintf("pvccopy-source-%x", hash[:12])
	if SourceHelperName(p.Source) != want {
		t.Fatal("source name digest mismatch")
	}
	replacement := p.Source
	replacement.UID = "replacement"
	if SourceHelperName(replacement) == want {
		t.Fatal("UID not included in source name")
	}
	for _, phase := range []string{"Running", "Succeeded", "Failed"} {
		t.Run(phase, func(t *testing.T) {
			f := newFake(t, p)
			obj := consumer(p.Source, phase, true)
			metadata(obj)["name"] = want
			metadata(obj)["uid"] = "orphan-uid"
			metadata(obj)["annotations"] = map[string]any{ownerAnnotation: "foreign-or-previous-attempt"}
			f.pods[p.Source.Namespace+"/"+want] = obj
			if _, err := testExecutor(f, p).Execute(context.Background(), p); err == nil {
				t.Fatal("orphan adopted")
			}
			if len(f.createdPods) != 0 || f.deletedPods != 0 || f.exports != 0 || metadata(f.pods[p.Source.Namespace+"/"+want])["uid"] != "orphan-uid" {
				t.Fatal("orphan mutated or executed")
			}
		})
	}
}
