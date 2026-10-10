package mysqlcopy

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
)

type fakeKube struct {
	mu            sync.Mutex
	objects       map[string]map[string]any
	writes        []string
	next          int
	restored      []byte
	probeDisabled bool
	probeRBAC     bool
}

func key(kind, ns, n string) string { return kind + "/" + ns + "/" + n }
func clone(m map[string]any) map[string]any {
	b, _ := json.Marshal(m)
	var r map[string]any
	_ = json.Unmarshal(b, &r)
	return r
}
func initialKube(p Plan) *fakeKube {
	k := &fakeKube{objects: map[string]map[string]any{}}
	put := func(kind string, r Ref, spec object, status object) {
		m := object{"metadata": object{"name": r.Name, "namespace": r.Namespace, "uid": r.UID}, "spec": spec}
		if status != nil {
			m["status"] = status
		}
		k.objects[key(kind, r.Namespace, r.Name)] = clone(m)
	}
	put("namespace", Ref{Name: p.Source.PVC.Namespace, UID: "ns-source"}, nil, nil)
	put("namespace", Ref{Name: p.Target.Namespace, UID: "ns-target"}, nil, nil)
	put("pvc", p.Source.PVC, object{"volumeMode": "Filesystem"}, object{"phase": "Bound"})
	put("statefulset", p.Source.StatefulSet, object{"replicas": 1}, nil)
	asMap(k.objects[key("statefulset", p.Source.StatefulSet.Namespace, p.Source.StatefulSet.Name)]["spec"])["template"] = map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": p.Source.Container, "image": p.Source.Image}}}}
	put("pod", Ref{p.Source.PVC.Namespace, p.Source.StatefulSet.Name + "-0", "source-pod"}, object{"volumes": []any{object{"name": "data", "persistentVolumeClaim": object{"claimName": p.Source.PVC.Name}}}, "containers": []any{object{"name": p.Source.Container, "image": p.Source.Image, "env": []any{object{"name": "MYSQL_DATABASE", "value": p.Source.Database}, object{"name": "MYSQL_USER", "value": p.Source.Credentials.Username}, object{"name": "MYSQL_PASSWORD", "valueFrom": object{"secretKeyRef": object{"name": p.Source.Credentials.Ref.Name, "key": p.Source.Credentials.PasswordKey}}}}}}}, object{"phase": "Running"})
	pod := k.objects[key("pod", p.Source.PVC.Namespace, p.Source.StatefulSet.Name+"-0")]
	asMap(pod["status"])["containerStatuses"] = []any{map[string]any{"name": p.Source.Container, "imageID": "docker-pullable://" + p.Source.Image, "ready": true, "state": map[string]any{"running": map[string]any{}}}}
	asMap(arr(pod, "spec", "containers")[0])["volumeMounts"] = []any{map[string]any{"name": "data", "mountPath": "/var/lib/mysql"}}
	asMap(pod["metadata"])["labels"] = map[string]any{"app": "mysql"}
	asMap(pod["metadata"])["ownerReferences"] = []any{map[string]any{"uid": p.Source.StatefulSet.UID, "kind": "StatefulSet"}}
	put("service", Ref{p.Source.PVC.Namespace, p.Source.Service, p.Source.ServiceUID}, object{"selector": object{"app": "mysql"}, "ports": []any{object{"port": p.Source.Port}}}, nil)
	put("endpoints", Ref{p.Source.PVC.Namespace, p.Source.Service, "endpoints-source"}, nil, nil)
	k.objects[key("endpoints", p.Source.PVC.Namespace, p.Source.Service)]["subsets"] = []any{map[string]any{"addresses": []any{map[string]any{"targetRef": map[string]any{"uid": "source-pod"}}}}}
	for _, r := range []CredentialRef{p.Source.Credentials, p.Source.BackupAdminSecretRef, p.Target.Credentials} {
		put("secret", r.Ref, nil, nil)
		k.objects[key("secret", r.Ref.Namespace, r.Ref.Name)]["data"] = map[string]any{r.PasswordKey: base64.StdEncoding.EncodeToString([]byte(r.Ref.UID))}
	}
	asMap(k.objects[key("secret", p.Target.Namespace, p.Target.Credentials.Ref.Name)]["data"])[p.Target.RootPasswordKey] = base64.StdEncoding.EncodeToString([]byte("independent-target-root-fixture"))
	put("secret", p.Source.TLSCASecret, nil, nil)
	k.objects[key("secret", p.Source.TLSCASecret.Namespace, p.Source.TLSCASecret.Name)]["data"] = map[string]any{p.Source.TLSCAKey: base64.StdEncoding.EncodeToString([]byte("approved CA fixture"))}
	put("validatingadmissionpolicy", Ref{Name: "sql-helper", UID: "policy-uid"}, object{"failurePolicy": "Fail", "validations": []any{object{"expression": "reviewed test protocol", "message": "envplane MySQL copy source fence [sha256:" + strings.Repeat("e", 64) + "]"}}}, object{"observedGeneration": 1, "typeChecking": object{"expressionWarnings": []any{}}})
	asMap(k.objects[key("validatingadmissionpolicy", "", "sql-helper")]["metadata"])["generation"] = float64(1)
	put("validatingadmissionpolicybinding", Ref{Name: "sql-helper", UID: "binding-uid"}, object{"policyName": "sql-helper", "validationActions": []any{"Deny"}}, nil)
	return k
}
func fixtureDriver(k *fakeKube, p Plan) *NativeDriver {
	fence := AdmissionFenceRef{Namespace: p.Source.PVC.Namespace, PolicyName: "sql-helper", PolicyUID: "policy-uid", BindingName: "sql-helper", BindingUID: "binding-uid", PolicySpecSHA256: specDigest(k.objects[key("validatingadmissionpolicy", "", "sql-helper")]), BindingSpecSHA256: specDigest(k.objects[key("validatingadmissionpolicybinding", "", "sql-helper")])}
	return NewProofDriver(DriverConfig{Commands: k, RunnerImage: p.ConfigImage, MySQLImage: p.Source.Image, ServiceAccount: "default", AllowRootInit: true, Authorize: func(context.Context, Plan, Stage) error { return nil }, SourceAdmissionFences: []AdmissionFenceRef{fence}})
}
func flag(args []string, f string) string {
	for i, v := range args {
		if v == f && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
func (k *fakeKube) Run(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	switch args[0] {
	case "get":
		k.mu.Lock()
		defer k.mu.Unlock()
		ns := flag(args, "-n")
		kind := args[1]
		if len(args) > 2 && args[2] == "-n" {
			items := []any{}
			for objectKey, v := range k.objects {
				if strings.HasPrefix(objectKey, key("pod", ns, "")) {
					items = append(items, v)
				}
			}
			return json.NewEncoder(out).Encode(object{"items": items})
		}
		if v := k.objects[key(kind, ns, args[2])]; v != nil {
			return json.NewEncoder(out).Encode(v)
		}
		for _, a := range args {
			if a == "--ignore-not-found" {
				return nil
			}
		}
		return ErrOperation
	case "create":
		var o map[string]any
		if json.NewDecoder(in).Decode(&o) != nil {
			return ErrUnsafe
		}
		for _, arg := range args {
			if arg == "--dry-run=server" {
				return json.NewEncoder(out).Encode(o)
			}
		}
		k.mu.Lock()
		defer k.mu.Unlock()
		kind := strings.ToLower(str(o, "kind"))
		if kind == "persistentvolumeclaim" {
			kind = "pvc"
		}
		meta := asMap(o["metadata"])
		k.next++
		if str(o, "metadata", "name") == "" {
			meta["name"] = str(o, "metadata", "generateName") + fmt.Sprint(k.next)
			delete(meta, "generateName")
		}
		meta["uid"] = fmt.Sprintf("created-%d", k.next)
		objectKey := key(kind, str(o, "metadata", "namespace"), str(o, "metadata", "name"))
		if k.objects[objectKey] != nil {
			return ErrPartial
		}
		if kind == "pod" {
			o["status"] = object{"phase": "Running", "containerStatuses": []any{object{"name": "mysql", "state": object{"running": object{}}}}}
		}
		k.objects[objectKey] = clone(o)
		k.writes = append(k.writes, "create "+objectKey)
		return json.NewEncoder(out).Encode(o)
	case "delete":
		var deletion map[string]any
		if json.NewDecoder(in).Decode(&deletion) != nil {
			return ErrUnsafe
		}
		parts := strings.Split(flag(args, "--raw"), "/")
		if len(parts) != 7 {
			return ErrUnsafe
		}
		k.mu.Lock()
		defer k.mu.Unlock()
		objectKey := key("pod", parts[4], parts[6])
		if v := k.objects[objectKey]; v == nil || str(v, "metadata", "uid") != str(deletion, "preconditions", "uid") {
			return ErrUnsafe
		}
		delete(k.objects, objectKey)
		k.writes = append(k.writes, "delete "+objectKey)
		return nil
	case "exec":
		ns := flag(args, "-n")
		n := ""
		for i, v := range args {
			if v == "-n" {
				n = args[i+2]
			}
		}
		i := 0
		for i < len(args) && args[i] != "--" {
			i++
		}
		if i+1 >= len(args) {
			return ErrUnsafe
		}
		cmd := args[i+1:]
		if cmd[0] == "kill" {
			k.mu.Lock()
			defer k.mu.Unlock()
			pod := k.objects[key("pod", ns, n)]
			pod["status"] = object{"phase": "Succeeded", "containerStatuses": []any{object{"name": "mysql", "state": object{"terminated": object{"exitCode": 0}}}}}
			return nil
		}
		if cmd[0] == "mysqldump" {
			k.mu.Lock()
			b := []byte(fixtureDump)
			if ns == "preview" {
				b = append([]byte(nil), k.restored...)
			}
			k.mu.Unlock()
			_, err := out.Write(b)
			return err
		}
		if strings.Contains(strings.Join(cmd, " "), "/config/admin.cnf") {
			scan := bufio.NewScanner(in)
			for scan.Scan() {
				line := scan.Text()
				if line == "SHOW GRANTS FOR CURRENT_USER;" {
					if _, err := io.WriteString(out, "GRANT SELECT, SHOW VIEW, TRIGGER, EVENT ON `app`.* TO `backupadmin`@`localhost`\nGRANT SHOW_ROUTINE, BACKUP_ADMIN ON *.* TO `backupadmin`@`localhost`\n"); err != nil {
						return err
					}
				}
				if strings.Contains(line, "SELECT (SELECT COUNT(*)") {
					if _, err := io.WriteString(out, "0\n"); err != nil {
						return err
					}
				}
				if strings.Contains(line, "SELECT t.table_name") {
					if _, err := io.WriteString(out, "records\tInnoDB\t1\n"); err != nil {
						return err
					}
				}
				if strings.HasPrefix(line, "SELECT 'envplane_lock_") {
					marker := strings.Split(line, "'")[1]
					if _, err := fmt.Fprintf(out, "%s\t42\tserver-uuid\n", marker); err != nil {
						return err
					}
				}
			}
			return scan.Err()
		}
		sql, err := io.ReadAll(in)
		if err != nil {
			return err
		}
		s := string(sql)
		answer := ""
		switch {
		case strings.Contains(s, "@@server_uuid, VERSION()"):
			answer = "server-uuid\t8.0.46\n"
		case strings.Contains(s, "SELECT @@server_uuid"):
			answer = "server-uuid\n"
		case strings.Contains(s, "SELECT (SELECT COUNT(*)"):
			answer = "0\n"
		case strings.Contains(s, "SELECT t.table_name"):
			answer = "records\tInnoDB\t1\n"
		case strings.TrimSpace(s) == "SELECT 1;":
			answer = "1\n"
		case ns == "preview":
			k.mu.Lock()
			k.restored = []byte(strings.TrimSuffix(strings.TrimPrefix(s, "SET SESSION SQL_MODE='NO_AUTO_VALUE_ON_ZERO'; SET SESSION TIME_ZONE='+00:00'; SET SESSION FOREIGN_KEY_CHECKS=0;\n"), "\nSET SESSION FOREIGN_KEY_CHECKS=1;\n"))
			k.mu.Unlock()
		default:
			return ErrUnsafe
		}
		_, err = io.WriteString(out, answer)
		return err
	}
	return ErrUnsafe
}
func TestNativeDriverEndToEndAndRestart(t *testing.T) {
	p := testPlan()
	k := initialKube(p)
	run := func() Receipt {
		d := fixtureDriver(k, p)
		transport := &KubeTransport{Commands: k, Driver: d, SourceNamespaces: []string{"source"}, RunnerImage: p.ConfigImage, MySQLImage: p.Source.Image}
		r, err := (DomainExecutor{Transport: transport, Authority: d.cfg.Authorize}).ExecuteDomain(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := run()
	if r.Proof.Bytes != int64(len(fixtureDump)) || !bytesEqual(k.restored, []byte(fixtureDump)) {
		t.Fatal("native copy bytes differ")
	}
	again := run()
	if !reflect.DeepEqual(r, again) {
		t.Fatal("completed restart changed immutable receipt")
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.writes) == 0 {
		t.Fatal("no native operations")
	}
	for objectKey := range k.objects {
		if strings.HasPrefix(objectKey, "pod/") && !strings.HasSuffix(objectKey, "/mysql-0") {
			t.Fatal("helper not UID cleaned", objectKey)
		}
	}
}
func bytesEqual(a, b []byte) bool { return string(a) == string(b) }
func (k *fakeKube) RunAdmissionFenceProbe(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	dry := false
	for _, arg := range args {
		if arg == "--dry-run=server" {
			dry = true
		}
	}
	if !dry {
		return ErrUnsafe
	}
	var pod map[string]any
	if json.NewDecoder(in).Decode(&pod) != nil || len(arr(pod, "spec", "volumes")) != 5 {
		return ErrUnsafe
	}
	if k.probeDisabled {
		return nil
	}
	if k.probeRBAC {
		_, err := io.WriteString(out, "Forbidden: RBAC denied create\n")
		if err != nil {
			return err
		}
		return ErrOperation
	}
	message := "ValidatingAdmissionPolicy 'sql-helper' with binding 'sql-helper' denied request: envplane MySQL copy source fence [sha256:" + strings.Repeat("e", 64) + "]"
	_, err := io.WriteString(out, message+"\n")
	if err != nil {
		return err
	}
	return ErrUnsafe
}
func TestAdmissionEnforcementNotStatus(t *testing.T) {
	for _, mode := range []string{"disabled", "RBAC"} {
		t.Run(mode, func(t *testing.T) {
			p := testPlan()
			k := initialKube(p)
			k.probeDisabled = mode == "disabled"
			k.probeRBAC = mode == "RBAC"
			d := fixtureDriver(k, p)
			if _, _, err := d.Source(context.Background(), p); err == nil || len(k.writes) != 0 {
				t.Fatal("status or unrelated denial accepted")
			}
		})
	}
}
func TestNativeSourceNegativeAuthority(t *testing.T) {
	p := testPlan()
	for _, change := range []func(*fakeKube){func(k *fakeKube) { asMap(k.objects[key("namespace", "", "source")]["metadata"])["uid"] = "recreated" }, func(k *fakeKube) { asMap(k.objects[key("pvc", "source", "data")]["metadata"])["uid"] = "foreign" }, func(k *fakeKube) {
		asMap(k.objects[key("validatingadmissionpolicy", "", "sql-helper")]["spec"])["failurePolicy"] = "Ignore"
	}, func(k *fakeKube) {
		asMap(k.objects[key("service", "source", "mysql")]["metadata"])["uid"] = "foreign-service"
	}} {
		k := initialKube(p)
		d := fixtureDriver(k, p)
		if err := d.pinNamespaces(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		change(k)
		if _, _, err := d.Source(context.Background(), p); err == nil || len(k.writes) != 0 {
			t.Fatal("source refusal did not precede writes")
		}
	}
}
func TestHelperReadbackRejectsMutation(t *testing.T) {
	p := testPlan()
	expected, _ := SourceClientHelperSpec(p, "default")
	for _, change := range []func(map[string]any){func(m map[string]any) { m["hostNetwork"] = true }, func(m map[string]any) { m["automountServiceAccountToken"] = true }, func(m map[string]any) { c := asMap(arr(m, "containers")[0]); c["image"] = "mysql:latest" }, func(m map[string]any) {
		m["volumes"] = append(arr(m, "volumes"), map[string]any{"name": "extra", "secret": map[string]any{"secretName": "arbitrary"}})
	}, func(m map[string]any) { asMap(asMap(arr(m, "containers")[0])["securityContext"])["privileged"] = true }} {
		actual := clone(expected)
		change(actual)
		if podMatches(expected, actual) {
			t.Fatal("unsafe admitted spec")
		}
	}
	actual := clone(expected)
	actual["nodeName"] = "node"
	actual["serviceAccount"] = "default"
	if !podMatches(expected, actual) {
		t.Fatal("harmless API defaults refused")
	}
}
func TestResolvedSourceImage(t *testing.T) {
	p := testPlan()
	k := initialKube(p)
	pod := k.objects[key("pod", "source", "mysql-0")]
	asMap(arr(pod, "spec", "containers")[0])["image"] = "mysql:8.0"
	asMap(arr(k.objects[key("statefulset", "source", "mysql")], "spec", "template", "spec", "containers")[0])["image"] = "mysql:8.0"
	d := fixtureDriver(k, p)
	if _, err := d.sourceReadback(context.Background(), p); err != nil {
		t.Fatal("reviewed platform pin refused", err)
	}
	asMap(arr(pod, "status", "containerStatuses")[0])["imageID"] = "docker-pullable://mysql@sha256:" + strings.Repeat("f", 64)
	if _, err := d.sourceReadback(context.Background(), p); err == nil {
		t.Fatal("unknown index/platform mapping accepted")
	}
}
