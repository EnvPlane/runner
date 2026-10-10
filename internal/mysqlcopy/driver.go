package mysqlcopy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/envplane/runner/internal/pvccopy"
)

// DriverConfig is trusted deployment configuration. Authorize enforces reviewed
// source policy, current namespace ownership, permissions and the leased command
// prepublication gate. No callback can skip native object/spec/consumer checks.
type DriverConfig struct {
	Commands                                pvccopy.CommandRunner
	RunnerImage, MySQLImage, ServiceAccount string
	AllowRootInit                           bool
	Authorize                               Authority
	QuiescenceHold                          func(context.Context, Plan) (Hold, error)
	SourceAdmissionFences                   []AdmissionFenceRef
}
type NativeDriver struct {
	validateMu          sync.Mutex // dump/restore preflights run concurrently; SQL streams do not hold this
	namespaceMu         sync.Mutex // concurrent teardown polls share immutable namespace UID pins
	cfg                 DriverConfig
	planDigest          string
	pods                map[string]ownedPod
	target              TargetIdentity
	restarting          bool
	namespaces          map[string]string
	admissionProven     bool
	declaredSourceImage string
	sourceServerUUID    string
}
type ownedPod struct {
	endpoint Endpoint
	spec     map[string]any
	target   bool
}

func NewProofDriver(cfg DriverConfig) *NativeDriver {
	return &NativeDriver{cfg: cfg, pods: map[string]ownedPod{}, namespaces: map[string]string{}}
}

type object map[string]any

func nested(m map[string]any, keys ...string) any {
	var v any = m
	for _, key := range keys {
		a, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = a[key]
	}
	return v
}
func str(m map[string]any, keys ...string) string  { s, _ := nested(m, keys...).(string); return s }
func arr(m map[string]any, keys ...string) []any   { a, _ := nested(m, keys...).([]any); return a }
func num(m map[string]any, keys ...string) float64 { n, _ := nested(m, keys...).(float64); return n }
func asMap(v any) map[string]any                   { m, _ := v.(map[string]any); return m }
func (d *NativeDriver) run(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	r := d.cfg.Commands
	if r == nil {
		r = pvccopy.OSKubectl{}
	}
	return safeError(ctx, r.Run(ctx, args, in, out))
}
func (d *NativeDriver) json(ctx context.Context, args []string, in any) (map[string]any, error) {
	var r io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return nil, ErrUnsafe
		}
		r = bytes.NewReader(b)
	}
	b := &limitBuffer{max: 4 << 20}
	if err := d.run(ctx, args, r, b); err != nil {
		return nil, err
	}
	var out map[string]any
	if json.Unmarshal(b.Bytes(), &out) != nil {
		return nil, ErrUnsafe
	}
	return out, nil
}
func (d *NativeDriver) get(ctx context.Context, kind string, r Ref) (map[string]any, error) {
	o, err := d.json(ctx, []string{"get", kind, r.Name, "-n", r.Namespace, "-o", "json"}, nil)
	if err != nil {
		return nil, err
	}
	if r.UID != "" && str(o, "metadata", "uid") != r.UID {
		return nil, ErrUnsafe
	}
	if nested(o, "metadata", "deletionTimestamp") != nil {
		return nil, ErrUnsafe
	}
	return o, nil
}
func (d *NativeDriver) gate(ctx context.Context, p Plan, s Stage) error {
	digest, err := p.Digest()
	if err != nil || d.cfg.Authorize == nil || !d.cfg.AllowRootInit || d.cfg.RunnerImage != p.ConfigImage || d.cfg.MySQLImage != p.Source.Image || !name.MatchString(d.cfg.ServiceAccount) {
		return ErrUnsafe
	}
	if d.planDigest != "" && d.planDigest != digest {
		return ErrUnsafe
	}
	d.planDigest = digest
	if d.cfg.Authorize(ctx, p, s) != nil {
		return ErrUnsafe
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return d.pinNamespaces(ctx, p)
}
func (d *NativeDriver) pinNamespaces(ctx context.Context, p Plan) error {
	d.namespaceMu.Lock()
	defer d.namespaceMu.Unlock()
	for _, ns := range []string{p.Source.PVC.Namespace, p.Target.Namespace} {
		o, err := d.json(ctx, []string{"get", "namespace", ns, "-o", "json"}, nil)
		if err != nil || str(o, "metadata", "uid") == "" || nested(o, "metadata", "deletionTimestamp") != nil {
			return ErrUnsafe
		}
		uid := str(o, "metadata", "uid")
		if old := d.namespaces[ns]; old != "" && old != uid {
			return ErrUnsafe
		}
		d.namespaces[ns] = uid
	}
	return nil
}
func (d *NativeDriver) sourceReadback(ctx context.Context, p Plan) (string, error) {
	if err := d.gate(ctx, p, ReadSource); err != nil {
		return "", err
	}
	if err := d.pinNamespaces(ctx, p); err != nil {
		return "", err
	}
	pvc, err := d.get(ctx, "pvc", p.Source.PVC)
	if err != nil || str(pvc, "status", "phase") != "Bound" || (str(pvc, "spec", "volumeMode") != "" && str(pvc, "spec", "volumeMode") != "Filesystem") {
		return "", ErrUnsafe
	}
	sts, err := d.get(ctx, "statefulset", p.Source.StatefulSet)
	if err != nil || num(sts, "spec", "replicas") != 1 {
		return "", ErrUnsafe
	}
	pod, err := d.get(ctx, "pod", Ref{Namespace: p.Source.PVC.Namespace, Name: p.Source.StatefulSet.Name + "-0"})
	if err != nil || str(pod, "status", "phase") != "Running" {
		return "", ErrUnsafe
	}
	owners := arr(pod, "metadata", "ownerReferences")
	if len(owners) != 1 || str(asMap(owners[0]), "uid") != p.Source.StatefulSet.UID || str(asMap(owners[0]), "kind") != "StatefulSet" {
		return "", ErrUnsafe
	}
	foundPVC := false
	volumeName := ""
	for _, v := range arr(pod, "spec", "volumes") {
		if str(asMap(v), "persistentVolumeClaim", "claimName") == p.Source.PVC.Name {
			foundPVC = true
			if volumeName != "" {
				return "", ErrUnsafe
			}
			volumeName = str(asMap(v), "name")
		}
	}
	if !foundPVC {
		return "", ErrUnsafe
	}
	var container map[string]any
	for _, v := range arr(pod, "spec", "containers") {
		c := asMap(v)
		if str(c, "name") == p.Source.Container {
			container = c
		}
	}
	if container == nil || str(container, "image") == "" {
		return "", ErrUnsafe
	}
	effectiveUID := nested(container, "securityContext", "runAsUser")
	if effectiveUID == nil {
		effectiveUID = nested(pod, "spec", "securityContext", "runAsUser")
	}
	if effectiveUID != nil && effectiveUID != float64(0) && effectiveUID != float64(999) {
		return "", ErrUnsafe
	}
	declared := str(container, "image")
	if d.declaredSourceImage != "" && d.declaredSourceImage != declared {
		return "", ErrUnsafe
	}
	statuses := 0
	for _, v := range arr(pod, "status", "containerStatuses") {
		s := asMap(v)
		if str(s, "name") == p.Source.Container {
			statuses++
			if nested(s, "ready") != true || nested(s, "state", "running") == nil || !resolvedImageMatches(p.Source.Image, str(s, "imageID")) {
				return "", ErrUnsafe
			}
		}
	}
	if statuses != 1 {
		return "", ErrUnsafe
	}
	d.declaredSourceImage = declared
	templateMatch := false
	for _, v := range arr(sts, "spec", "template", "spec", "containers") {
		c := asMap(v)
		if str(c, "name") == p.Source.Container && str(c, "image") == declared {
			templateMatch = true
		}
	}
	if !templateMatch {
		return "", ErrUnsafe
	}
	if len(arr(container, "command")) != 0 || len(arr(container, "args")) != 0 {
		return "", ErrUnsafe
	}
	datadir := false
	for _, mount := range arr(container, "volumeMounts") {
		if str(asMap(mount), "mountPath") == "/var/lib/mysql" && str(asMap(mount), "name") == volumeName {
			datadir = true
		}
	}
	if !datadir {
		return "", ErrUnsafe
	}
	env := map[string]map[string]any{}
	for _, v := range arr(container, "env") {
		e := asMap(v)
		if env[str(e, "name")] != nil {
			return "", ErrUnsafe
		}
		env[str(e, "name")] = e
	}
	if str(env["MYSQL_DATABASE"], "value") != p.Source.Database {
		return "", ErrUnsafe
	}
	cred := p.Source.Credentials
	if cred.UsernameKey == "" {
		if str(env["MYSQL_USER"], "value") != cred.Username {
			return "", ErrUnsafe
		}
	} else if str(env["MYSQL_USER"], "valueFrom", "secretKeyRef", "name") != cred.Ref.Name || str(env["MYSQL_USER"], "valueFrom", "secretKeyRef", "key") != cred.UsernameKey {
		return "", ErrUnsafe
	}
	if str(env["MYSQL_PASSWORD"], "valueFrom", "secretKeyRef", "name") != cred.Ref.Name || str(env["MYSQL_PASSWORD"], "valueFrom", "secretKeyRef", "key") != cred.PasswordKey {
		return "", ErrUnsafe
	}
	service, err := d.get(ctx, "service", Ref{p.Source.PVC.Namespace, p.Source.Service, p.Source.ServiceUID})
	if err != nil || str(service, "spec", "type") == "ExternalName" {
		return "", ErrUnsafe
	}
	selector := asMap(nested(service, "spec", "selector"))
	if len(selector) == 0 {
		return "", ErrUnsafe
	}
	for key, v := range selector {
		if nested(pod, "metadata", "labels", key) != v {
			return "", ErrUnsafe
		}
	}
	portOK := false
	for _, v := range arr(service, "spec", "ports") {
		if num(asMap(v), "port") == float64(p.Source.Port) {
			portOK = true
		}
	}
	if !portOK {
		return "", ErrUnsafe
	}
	// Endpoints must resolve solely to the reviewed Pod. Selector labels alone
	// cannot prove the address reached by the SQL client.
	endpoints, err := d.get(ctx, "endpoints", Ref{Namespace: p.Source.PVC.Namespace, Name: p.Source.Service})
	if err != nil {
		return "", ErrUnsafe
	}
	count := 0
	for _, v := range arr(endpoints, "subsets") {
		sub := asMap(v)
		if len(arr(sub, "notReadyAddresses")) != 0 {
			return "", ErrUnsafe
		}
		for _, address := range arr(sub, "addresses") {
			a := asMap(address)
			if str(a, "targetRef", "uid") != str(pod, "metadata", "uid") {
				return "", ErrUnsafe
			}
			count++
		}
	}
	if count != 1 {
		return "", ErrUnsafe
	}
	return str(pod, "metadata", "uid"), nil
}
func resolvedImageMatches(pin, actual string) bool {
	suffix := strings.SplitN(pin, "@", 2)
	if len(suffix) != 2 {
		return false
	}
	if strings.HasPrefix(actual, "containerd://sha256:") {
		return strings.TrimPrefix(actual, "containerd://") == suffix[1]
	}
	actual = strings.TrimPrefix(actual, "docker-pullable://")
	parts := strings.SplitN(actual, "@", 2)
	return len(parts) == 2 && parts[1] == suffix[1]
}
func (d *NativeDriver) Source(ctx context.Context, p Plan) (Endpoint, string, error) {
	uid, err := d.sourceReadback(ctx, p)
	if err != nil {
		return Endpoint{}, "", err
	}
	if err := d.requireAdmission(ctx, p); err != nil {
		return Endpoint{}, "", err
	}
	for _, owned := range d.pods {
		if !owned.target {
			return owned.endpoint, uid, d.Validate(ctx, p, owned.endpoint, false)
		}
	}
	spec := helperSpec(p, d.cfg.ServiceAccount, false, false, "")
	ep, err := d.createPod(ctx, p, false, spec)
	return ep, uid, err
}
func labels(p Plan) map[string]any {
	return map[string]any{"envplane.io/managed": "true", "envplane.io/project": p.Project, "envplane.io/environment": p.Environment, "envplane.io/mysql-copy-plan": hashString([]byte(p.Binding))[:24]}
}
func (d *NativeDriver) createPod(ctx context.Context, p Plan, target bool, spec map[string]any) (Endpoint, error) {
	ns := p.Source.PVC.Namespace
	prefix := "mysqlcopy-client-"
	if target {
		ns = p.Target.Namespace
		prefix = "mysqlcopy-target-"
	}
	body := object{"apiVersion": "v1", "kind": "Pod", "metadata": object{"generateName": prefix, "namespace": ns, "labels": labels(p), "annotations": object{"envplane.io/mysql-copy-digest": d.planDigest}}, "spec": spec}
	if !target {
		meta := body["metadata"].(object)
		delete(meta, "generateName")
		meta["name"] = SourceClientHelperName(p.Source.PVC)
	}
	o, err := d.json(ctx, []string{"create", "-f", "-", "-o", "json"}, body)
	if err != nil {
		return Endpoint{}, err
	}
	ep := Endpoint{Ref{ns, str(o, "metadata", "name"), str(o, "metadata", "uid")}, "mysql"}
	if !validRef(ep.Pod) {
		return Endpoint{}, ErrUnsafe
	}
	d.pods[ep.Pod.UID] = ownedPod{ep, spec, target}
	if d.Validate(ctx, p, ep, target) != nil {
		return ep, ErrUnsafe
	}
	if err := d.waitRunning(ctx, p, ep, target); err != nil {
		return ep, err
	}
	return ep, nil
}

// SourceClientHelperName is stable for narrowly resourceName-bound exec/delete.
// A pre-existing orphan is never adopted or deleted, even if its name matches.
func SourceClientHelperName(ref Ref) string {
	return "mysqlcopy-source-" + hashString([]byte(ref.Namespace + "/" + ref.Name + "/" + ref.UID))[:24]
}

// SourceClientHelperSpec exposes the exact reviewed no-PVC helper protocol for
// onboarding/admission generators; callers must not treat a manifest as proof.
func SourceClientHelperSpec(p Plan, serviceAccount string) (map[string]any, error) {
	if p.Validate() != nil || !name.MatchString(serviceAccount) {
		return nil, ErrUnsafe
	}
	return helperSpec(p, serviceAccount, false, false, ""), nil
}
func (d *NativeDriver) waitRunning(ctx context.Context, p Plan, ep Endpoint, target bool) error {
	for {
		if err := d.Validate(ctx, p, ep, target); err != nil {
			return err
		}
		o, err := d.get(ctx, "pod", ep.Pod)
		if err != nil {
			return err
		}
		phase := str(o, "status", "phase")
		if phase == "Failed" || phase == "Succeeded" {
			return ErrUnsafe
		}
		if phase == "Running" {
			ready := false
			for _, v := range arr(o, "status", "containerStatuses") {
				if str(asMap(v), "name") == "mysql" && nested(asMap(v), "state", "running") != nil {
					ready = true
				}
			}
			if ready {
				return nil
			}
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
func (d *NativeDriver) targetConsumers(ctx context.Context, p Plan, exempt string) error {
	list, err := d.json(ctx, []string{"get", "pods", "-n", p.Target.Namespace, "-o", "json"}, nil)
	if err != nil {
		return err
	}
	for _, v := range arr(list, "items") {
		pod := asMap(v)
		for _, volume := range arr(pod, "spec", "volumes") {
			if str(asMap(volume), "persistentVolumeClaim", "claimName") == p.Target.PVCName && str(pod, "metadata", "uid") != exempt {
				return ErrUnsafe
			}
		}
	}
	return nil
}
func (d *NativeDriver) Validate(ctx context.Context, p Plan, ep Endpoint, target bool) error {
	d.validateMu.Lock()
	defer d.validateMu.Unlock()
	stage := ReadSource
	if target {
		stage = Transfer
	}
	if err := d.gate(ctx, p, stage); err != nil {
		return err
	}
	if err := d.pinNamespaces(ctx, p); err != nil {
		return err
	}
	if _, err := d.sourceReadback(ctx, p); err != nil {
		return err
	}
	if err := d.requireAdmission(ctx, p); err != nil {
		return err
	}
	owned, ok := d.pods[ep.Pod.UID]
	if !ok || owned.endpoint != ep || owned.target != target {
		return ErrUnsafe
	}
	pod, err := d.get(ctx, "pod", ep.Pod)
	if err != nil {
		return err
	}
	actual := asMap(nested(pod, "spec"))
	if !podMatches(owned.spec, actual) {
		return ErrUnsafe
	}
	if nested(pod, "metadata", "deletionTimestamp") != nil {
		return ErrUnsafe
	}
	if str(pod, "metadata", "annotations", "envplane.io/mysql-copy-digest") != d.planDigest {
		return ErrUnsafe
	}
	if target {
		if err := d.targetConsumers(ctx, p, ep.Pod.UID); err != nil {
			return err
		}
		pvc, err := d.get(ctx, "pvc", Ref{p.Target.Namespace, p.Target.PVCName, d.target.PVCUID})
		if err != nil || str(pvc, "metadata", "annotations", "envplane.io/mysql-copy-digest") != d.planDigest || !targetPVCSpec(p, pvc) {
			return ErrUnsafe
		}
	}
	return nil
}
func targetPVCSpec(p Plan, pvc map[string]any) bool {
	return str(pvc, "spec", "storageClassName") == p.Target.StorageClass && str(pvc, "spec", "volumeMode") == "Filesystem" && reflect.DeepEqual(arr(pvc, "spec", "accessModes"), []any{"ReadWriteOnce"}) && nested(pvc, "spec", "dataSource") == nil && nested(pvc, "spec", "dataSourceRef") == nil
}

// Defaults from the API server are narrowly ignored. Unexpected fields in
// container/volume/security/environment subtrees are rejected, including any
// injected sidecar, extra Secret, hostPath, capability or service account token.
func podMatches(expected, actual map[string]any) bool {
	b, _ := json.Marshal(actual)
	var clean map[string]any
	if json.Unmarshal(b, &clean) != nil {
		return false
	}
	for _, key := range []string{"nodeName", "schedulerName", "priority", "preemptionPolicy", "dnsPolicy", "tolerations"} {
		delete(clean, key)
	}
	if alias := str(clean, "serviceAccount"); alias != "" {
		if alias != str(expected, "serviceAccountName") {
			return false
		}
		delete(clean, "serviceAccount")
	}
	for _, list := range []string{"containers", "initContainers"} {
		for _, v := range arr(clean, list) {
			c := asMap(v)
			delete(c, "terminationMessagePath")
			delete(c, "terminationMessagePolicy")
			delete(c, "imagePullPolicy")
		}
	}
	b, _ = json.Marshal(expected)
	var normalized map[string]any
	if json.Unmarshal(b, &normalized) != nil {
		return false
	}
	return reflect.DeepEqual(normalized, clean)
}
func (d *NativeDriver) Inspect(ctx context.Context, p Plan) (*Receipt, error) {
	if err := d.gate(ctx, p, ProvisionTarget); err != nil {
		return nil, err
	}
	if err := d.targetConsumers(ctx, p, ""); err != nil {
		return nil, err
	}
	// --ignore-not-found distinguishes absence from RBAC/transport failure.
	b := &limitBuffer{max: 1 << 20}
	if err := d.run(ctx, []string{"get", "pvc", p.Target.PVCName, "-n", p.Target.Namespace, "--ignore-not-found", "-o", "json"}, nil, b); err != nil {
		return nil, err
	}
	if b.Len() == 0 {
		return nil, nil
	}
	var pvc map[string]any
	if json.Unmarshal(b.Bytes(), &pvc) != nil || str(pvc, "metadata", "annotations", "envplane.io/mysql-copy-digest") != d.planDigest {
		return nil, ErrPartial
	}
	marker, err := d.get(ctx, "configmap", Ref{Namespace: p.Target.Namespace, Name: receiptName(p)})
	if err != nil || nested(marker, "immutable") != true {
		return nil, ErrPartial
	}
	var r Receipt
	if json.Unmarshal([]byte(str(marker, "data", "receipt.json")), &r) != nil || r.PlanDigest != d.planDigest || r.Target.PVCUID != str(pvc, "metadata", "uid") || r.Target.SecretUID != p.Target.Credentials.Ref.UID || !r.ShutdownVerified {
		return nil, ErrPartial
	}
	if r.Version != 1 || r.Binding != p.Binding || r.SourcePVCUID != p.Source.PVC.UID || r.SourceStatefulSetUID != p.Source.StatefulSet.UID || d.sourceServerUUID == "" || r.SourceServerUUID != d.sourceServerUUID || r.Target.PodUID == "" || !targetPVCSpec(p, pvc) {
		return nil, ErrPartial
	}
	d.restarting = true
	d.target = TargetIdentity{PVCUID: r.Target.PVCUID, SecretUID: r.Target.SecretUID}
	ep, err := d.createPod(ctx, p, true, helperSpec(p, d.cfg.ServiceAccount, true, true, d.target.PVCUID))
	if err != nil {
		return nil, err
	}
	d.target.PodUID = ep.Pod.UID
	if err := d.waitSQL(ctx, p, ep); err != nil {
		return nil, err
	}
	tracker := newTracker(p)
	if err := d.exec(ctx, p, ep, dumpArgs(p, true), nil, tracker); err != nil {
		return nil, err
	}
	proof, err := tracker.Finish()
	if err != nil || !sameProof(proof, r.Proof) {
		return nil, ErrPartial
	}
	if err := d.Shutdown(ctx, p, ep); err != nil {
		return nil, err
	}
	return &r, nil
}
func receiptName(p Plan) string {
	return "mysqlcopy-receipt-" + hashString([]byte(p.Binding + "/" + p.Target.PVCName))[:24]
}
func (d *NativeDriver) Target(ctx context.Context, p Plan) (Endpoint, TargetIdentity, error) {
	if err := d.gate(ctx, p, ProvisionTarget); err != nil {
		return Endpoint{}, TargetIdentity{}, err
	}
	if err := d.targetConsumers(ctx, p, ""); err != nil {
		return Endpoint{}, TargetIdentity{}, err
	}
	body := object{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": object{"name": p.Target.PVCName, "namespace": p.Target.Namespace, "labels": labels(p), "annotations": object{"envplane.io/mysql-copy-digest": d.planDigest}}, "spec": object{"accessModes": []string{"ReadWriteOnce"}, "storageClassName": p.Target.StorageClass, "volumeMode": "Filesystem", "resources": object{"requests": object{"storage": strconv.FormatInt(p.Target.RequestedBytes, 10)}}}}
	pvc, err := d.json(ctx, []string{"create", "-f", "-", "-o", "json"}, body)
	if err != nil {
		return Endpoint{}, TargetIdentity{}, err
	}
	uid := str(pvc, "metadata", "uid")
	if uid == "" || !targetPVCSpec(p, pvc) {
		return Endpoint{}, TargetIdentity{}, ErrUnsafe
	}
	d.target = TargetIdentity{PVCUID: uid, SecretUID: p.Target.Credentials.Ref.UID}
	ep, err := d.createPod(ctx, p, true, helperSpec(p, d.cfg.ServiceAccount, true, false, d.target.PVCUID))
	if err != nil {
		return ep, d.target, err
	}
	d.target.PodUID = ep.Pod.UID
	if err := d.waitSQL(ctx, p, ep); err != nil {
		return ep, d.target, err
	}
	return ep, d.target, nil
}
func (d *NativeDriver) exec(ctx context.Context, p Plan, ep Endpoint, args []string, in io.Reader, out io.Writer) error {
	if err := d.Validate(ctx, p, ep, true); err != nil {
		return err
	}
	a := []string{"exec", "-n", ep.Pod.Namespace, ep.Pod.Name, "-c", ep.Container, "-i", "--"}
	return d.run(ctx, append(a, args...), in, out)
}
func (d *NativeDriver) waitSQL(ctx context.Context, p Plan, ep Endpoint) error {
	for {
		b := &limitBuffer{max: 1024}
		err := d.exec(ctx, p, ep, clientArgs(p, true, false), strings.NewReader("SELECT 1;\n"), b)
		if err == nil && strings.TrimSpace(b.String()) == "1" {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
func (d *NativeDriver) Shutdown(ctx context.Context, p Plan, ep Endpoint) error {
	// Official MySQL images do not ship an external kill executable. Their
	// POSIX shell builtin signals the same-UID PID 1 without extra capabilities.
	if err := d.exec(ctx, p, ep, []string{"sh", "-c", "kill -TERM 1"}, nil, io.Discard); err != nil {
		return err
	}
	for {
		if err := d.Validate(ctx, p, ep, true); err != nil {
			return err
		}
		pod, err := d.get(ctx, "pod", ep.Pod)
		if err != nil {
			return err
		}
		for _, v := range arr(pod, "status", "containerStatuses") {
			c := asMap(v)
			if str(c, "name") == "mysql" && nested(c, "state", "terminated") != nil {
				if num(c, "state", "terminated", "exitCode") != 0 {
					return ErrUnsafe
				}
				return nil
			}
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
func (d *NativeDriver) Commit(ctx context.Context, p Plan, r Receipt) error {
	if err := d.gate(ctx, p, Complete); err != nil {
		return err
	}
	if r.Target != d.target || !r.ShutdownVerified {
		return ErrUnsafe
	}
	owned, ok := d.pods[r.Target.PodUID]
	if !ok {
		return ErrUnsafe
	}
	pod, err := d.get(ctx, "pod", owned.endpoint.Pod)
	if err != nil {
		return err
	}
	statuses := arr(pod, "status", "containerStatuses")
	if len(statuses) != 1 || nested(asMap(statuses[0]), "state", "terminated") == nil || num(asMap(statuses[0]), "state", "terminated", "exitCode") != 0 {
		return ErrUnsafe
	}
	if err := d.targetConsumers(ctx, p, r.Target.PodUID); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return ErrUnsafe
	}
	body := object{"apiVersion": "v1", "kind": "ConfigMap", "immutable": true, "metadata": object{"name": receiptName(p), "namespace": p.Target.Namespace, "labels": labels(p), "annotations": object{"envplane.io/mysql-copy-digest": d.planDigest}}, "data": object{"receipt.json": string(b)}}
	_, err = d.json(ctx, []string{"create", "-f", "-", "-o", "json"}, body)
	return err
}
func (d *NativeDriver) Cleanup(ctx context.Context, p Plan) error {
	var failures []error
	waiting := []ownedPod{}
	// Send every UID-preconditioned delete before waiting. All helpers retain
	// their full 30s grace; multiple PID1 sleepers must not serialize that grace.
	for uid, owned := range d.pods {
		if err := d.pinNamespaces(ctx, p); err != nil {
			if ctx.Err() != nil {
				err = ctx.Err()
			}
			return errors.Join(errors.Join(failures...), safeFailure(CleanupNamespace, err))
		}
		// Only Pods created by this in-memory execution are eligible. Target data,
		// receipts, approved Secrets and all pre-existing/orphan objects are retained.
		path := "/api/v1/namespaces/" + url.PathEscape(owned.endpoint.Pod.Namespace) + "/pods/" + url.PathEscape(owned.endpoint.Pod.Name)
		b, err := json.Marshal(object{"apiVersion": "v1", "kind": "DeleteOptions", "preconditions": object{"uid": uid}, "gracePeriodSeconds": 30})
		if err != nil {
			return safeFailure(CleanupHelpers, ErrUnsafe)
		}
		if err := d.run(ctx, []string{"delete", "--raw", path, "-f", "-"}, bytes.NewReader(b), io.Discard); err != nil {
			stage := DeleteSourceHelper
			if owned.target {
				stage = DeleteTargetHelper
			}
			failures = append(failures, safeFailure(stage, err))
		} else {
			waiting = append(waiting, owned)
		}
	}
	results := make(chan error, len(waiting))
	for _, owned := range waiting {
		go func() {
			stage := WaitSourceHelperDeleted
			if owned.target {
				stage = WaitTargetHelperDeleted
			}
			results <- safeFailure(stage, d.waitDeleted(ctx, p, owned.endpoint.Pod))
		}()
	}
	for range waiting {
		if err := <-results; err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
func (d *NativeDriver) waitDeleted(ctx context.Context, p Plan, ref Ref) error {
	for {
		if err := d.pinNamespaces(ctx, p); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		b := &limitBuffer{max: 1 << 20}
		if err := d.run(ctx, []string{"get", "pod", ref.Name, "-n", ref.Namespace, "--ignore-not-found", "-o", "json"}, nil, b); err != nil {
			return err
		}
		if b.Len() == 0 {
			return nil
		}
		var o map[string]any
		if json.Unmarshal(b.Bytes(), &o) != nil || str(o, "metadata", "uid") != ref.UID {
			return ErrUnsafe
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
func (d *NativeDriver) Quiescence(ctx context.Context, p Plan) (Hold, error) {
	if d.cfg.QuiescenceHold == nil || d.gate(ctx, p, HoldDDL) != nil {
		return nil, ErrUnsafe
	}
	return d.cfg.QuiescenceHold(ctx, p)
}

func helperSpec(p Plan, sa string, target, restart bool, pvcUID string) map[string]any {
	security := object{"runAsUser": float64(999), "runAsGroup": float64(999), "runAsNonRoot": true, "allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": object{"drop": []any{"ALL"}}}
	volumes := []any{object{"name": "config", "emptyDir": object{"medium": "Memory", "sizeLimit": "32Mi"}}, object{"name": "tmp", "emptyDir": object{"medium": "Memory", "sizeLimit": "64Mi"}}, object{"name": "run", "emptyDir": object{"medium": "Memory", "sizeLimit": "16Mi"}}}
	mounts := []any{object{"name": "config", "mountPath": "/config", "readOnly": true}, object{"name": "tmp", "mountPath": "/tmp"}, object{"name": "run", "mountPath": "/run/mysqld"}}
	cred := p.Source.Credentials
	if target {
		cred = p.Target.Credentials
	}
	sources := []any{}
	args := []any{"mysql-client-config"}
	addCred := func(r CredentialRef, prefix, out string) {
		items := []any{object{"key": r.PasswordKey, "path": prefix + ".password"}}
		user := r.Username
		if r.UsernameKey != "" {
			items = append(items, object{"key": r.UsernameKey, "path": prefix + ".user"})
			user = "@/credentials/" + prefix + ".user"
		}
		sources = append(sources, object{"secret": object{"name": r.Ref.Name, "items": items}})
		args = append(args, user, "/credentials/"+prefix+".password", out)
	}
	addCred(cred, "app", "/config/app.cnf")
	if !target && p.DDLMode == "backup_lock" {
		addCred(p.Source.BackupAdminSecretRef, "admin", "/config/admin.cnf")
	}
	if !target {
		sources = append(sources, object{"secret": object{"name": p.Source.TLSCASecret.Name, "items": []any{object{"key": p.Source.TLSCAKey, "path": "ca.pem", "mode": float64(0444)}}}})
	}
	volumes = append(volumes, object{"name": "credentials", "projected": object{"defaultMode": float64(0400), "sources": sources}})
	initMounts := []any{object{"name": "credentials", "mountPath": "/credentials", "readOnly": true}, object{"name": "config", "mountPath": "/config"}, object{"name": "tmp", "mountPath": "/tmp"}, object{"name": "run", "mountPath": "/run/mysqld"}}
	if !target {
		mounts = append(mounts, object{"name": "credentials", "mountPath": "/credentials", "readOnly": true})
	}
	initSecurity := object{"runAsUser": float64(0), "runAsGroup": float64(0), "runAsNonRoot": false, "allowPrivilegeEscalation": false, "readOnlyRootFilesystem": true, "capabilities": object{"drop": []any{"ALL"}, "add": []any{"CHOWN", "FOWNER", "DAC_OVERRIDE"}}}
	container := object{"name": "mysql", "image": p.Source.Image, "securityContext": security, "volumeMounts": mounts, "command": []any{"sleep"}, "args": []any{strconv.FormatInt(int64(p.Timeout/time.Second), 10)}, "resources": object{"requests": object{"cpu": "100m", "memory": "128Mi"}, "limits": object{"cpu": "1", "memory": "1Gi"}}}
	if target {
		digest, _ := p.Digest()
		mode := "--target-claim="
		if restart {
			mode = "--target-resume="
		}
		args = append([]any{args[0], mode + digest, "--target-uid=" + pvcUID}, args[1:]...)
		volumes = append(volumes, object{"name": "data", "persistentVolumeClaim": object{"claimName": p.Target.PVCName}})
		initMounts = append(initMounts, object{"name": "data", "mountPath": "/data"})
		mounts = append(mounts, object{"name": "data", "mountPath": "/var/lib/mysql", "subPath": TargetDataSubPath})
		container["volumeMounts"] = mounts
		container["command"] = []any{"docker-entrypoint.sh"}
		container["args"] = []any{"mysqld", "--skip-name-resolve"}
		userEnv := object{"name": "MYSQL_USER", "value": cred.Username}
		if cred.UsernameKey != "" {
			delete(userEnv, "value")
			userEnv["valueFrom"] = object{"secretKeyRef": object{"name": cred.Ref.Name, "key": cred.UsernameKey}}
		}
		// Do not request entrypoint-generated random root credentials: official
		// MySQL images print them to Pod logs. The isolated target's new root and
		// app passwords come from distinct keys of the approved generated Secret.
		container["env"] = []any{object{"name": "MYSQL_DATABASE", "value": p.Target.Database}, userEnv, object{"name": "MYSQL_PASSWORD", "valueFrom": object{"secretKeyRef": object{"name": cred.Ref.Name, "key": cred.PasswordKey}}}, object{"name": "MYSQL_ROOT_PASSWORD", "valueFrom": object{"secretKeyRef": object{"name": cred.Ref.Name, "key": p.Target.RootPasswordKey}}}}
	}
	return map[string]any{"restartPolicy": "Never", "automountServiceAccountToken": false, "serviceAccountName": sa, "activeDeadlineSeconds": float64(int64(p.Timeout/time.Second) + 30), "terminationGracePeriodSeconds": float64(30), "enableServiceLinks": false, "securityContext": object{}, "containers": []any{container}, "initContainers": []any{object{"name": "config", "image": p.ConfigImage, "command": []any{"envplane-runner"}, "args": args, "securityContext": initSecurity, "volumeMounts": initMounts, "resources": object{"requests": object{"cpu": "10m", "memory": "16Mi"}, "limits": object{"cpu": "100m", "memory": "64Mi"}}}}, "volumes": volumes}
}

var _ ProofDriver = (*NativeDriver)(nil)

func (d *NativeDriver) observeSource(s SourceIdentity) error {
	if d.sourceServerUUID != "" && d.sourceServerUUID != s.ServerUUID {
		return ErrUnsafe
	}
	d.sourceServerUUID = s.ServerUUID
	return nil
}
