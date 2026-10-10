// Package pvccopy implements bounded, offline filesystem copies, not database
// logical backups or CSI snapshots. Authorization and workload fencing belong
// to the caller; this package never changes source workloads or RBAC.
package pvccopy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"
)

const (
	MaxCopyBytes   int64 = 1 << 40
	MaxCopyEntries       = 100000
	MaxCopyTimeout       = 6 * time.Hour
	StateDirectory       = ".envplane-pvccopy"
	HelperBinary         = "/usr/local/bin/envplane-runner"
)

var (
	ErrUnsafe  = errors.New("unsafe PVC copy")
	ErrPartial = errors.New("partial or unknown target: operator intervention required")
	dnsName    = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?$`)
	imageRef   = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]*@sha256:[a-f0-9]{64}$`)
)

type PVCRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}

type TargetBinding struct {
	Tenant      string `json:"tenant"`
	Project     string `json:"project"`
	Environment string `json:"environment"`
	PVC         PVCRef `json:"pvc"`
}

// Plan must be authorized against trusted policy before Execute. Its allowlist
// and binding are policy input, not authorization obtained from Kubernetes.
// SourceQuiescedOffline asserts application shutdown AND externally held fencing
// against any new consumer of either PVC until Execute returns.
type Plan struct {
	ImmutablePlanDigest     string        `json:"immutablePlanDigest,omitempty"`
	ItemID                  string        `json:"itemId,omitempty"`
	Tenant                  string        `json:"tenant"`
	Project                 string        `json:"project"`
	Environment             string        `json:"environment"`
	Source                  PVCRef        `json:"source"`
	Target                  PVCRef        `json:"target"`
	MaxBytes                int64         `json:"maxBytes"`
	MaxEntries              int           `json:"maxEntries"`
	Timeout                 time.Duration `json:"timeout"`
	Image                   string        `json:"image"`
	AllowedSourceNamespaces []string      `json:"allowedSourceNamespaces"`
	AllowedTarget           TargetBinding `json:"allowedTarget"`
	SourceQuiescedOffline   bool          `json:"sourceQuiescedOffline"`
}

func (p Plan) Validate() error {
	bad := func(s string) error { return fmt.Errorf("%w: %s", ErrUnsafe, s) }
	for _, s := range []string{p.Tenant, p.Project, p.Environment, p.Source.UID, p.Target.UID} {
		if s == "" || len(s) > 256 {
			return bad("missing or oversized identity")
		}
	}
	for _, s := range []string{p.Source.Namespace, p.Source.Name, p.Target.Namespace, p.Target.Name} {
		if len(s) > 253 || !dnsName.MatchString(s) {
			return bad("invalid PVC reference")
		}
	}
	if !p.SourceQuiescedOffline {
		return bad("source is not fenced OFFLINE")
	}
	if p.Source.UID == p.Target.UID || (p.Source.Namespace == p.Target.Namespace && p.Source.Name == p.Target.Name) {
		return bad("source equals target")
	}
	if p.MaxBytes <= 0 || p.MaxBytes > MaxCopyBytes || p.MaxEntries <= 0 || p.MaxEntries > MaxCopyEntries || p.Timeout <= 0 || p.Timeout > MaxCopyTimeout {
		return bad("invalid bounds")
	}
	if !imageRef.MatchString(p.Image) {
		return bad("helper image must be sha256 pinned")
	}
	allowed := false
	if len(p.AllowedSourceNamespaces) > 1000 {
		return bad("oversized namespace policy")
	}
	for _, ns := range p.AllowedSourceNamespaces {
		if ns == p.Source.Namespace {
			allowed = true
		}
	}
	if !allowed {
		return bad("source namespace denied")
	}
	if p.AllowedTarget != (TargetBinding{p.Tenant, p.Project, p.Environment, p.Target}) {
		return bad("target binding denied")
	}
	return nil
}

func (p Plan) Digest() (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	p.AllowedSourceNamespaces = append([]string(nil), p.AllowedSourceNamespaces...)
	sort.Strings(p.AllowedSourceNamespaces)
	b, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

// RootReceipt makes the volume root's application-access semantics explicit.
// The same fields are also part of the canonical tree SHA256 via the first '.'
// directory header. Zero file bytes never imply missing root metadata.
type RootReceipt struct {
	UID  int    `json:"uid"`
	GID  int    `json:"gid"`
	Mode uint32 `json:"mode"`
}

type Receipt struct {
	Root    RootReceipt `json:"root"`
	Bytes   int64       `json:"bytes"`
	Entries int         `json:"entries"`
	SHA256  string      `json:"sha256"`
}

type Marker struct {
	Version             int     `json:"version"`
	PlanDigest          string  `json:"planDigest"`
	SourceUID           string  `json:"sourceUID"`
	TargetUID           string  `json:"targetUID"`
	Receipt             Receipt `json:"receipt"`
	ImmutablePlanDigest string  `json:"immutablePlanDigest,omitempty"`
}

func markerFor(p Plan, r Receipt) Marker {
	d, _ := p.Digest()
	return Marker{Version: 1, PlanDigest: d, SourceUID: p.Source.UID, TargetUID: p.Target.UID, Receipt: r, ImmutablePlanDigest: p.ImmutablePlanDigest}
}
