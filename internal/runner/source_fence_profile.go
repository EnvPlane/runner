package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/envplane/contracts/domain"
	"github.com/envplane/runner/internal/pvccopy"
	"io"
	"strings"
)

// Trusted immutable chart/deployment config ONLY. Do not load admission receipts
// from command.ProjectConfig or browser fields. Malformed present config refuses
// execution rather than silently falling back to the legacy FS-only policy.
func requireConfiguredPVCSourceFence(ctx context.Context, k *pvccopy.Kubectl, p domain.PVCCopyPlan, ns, sa string) error {
	refs, configured, err := configuredPVCSourceFences(getenv("ENVPLANE_PVC_COPY_SOURCE_FENCES", ""))
	if err != nil {
		return err
	}
	if configured {
		return k.RequireReviewedAdmissionFence(ctx, p, ns, sa, refs)
	}
	return k.RequireAdmissionFence(ctx, p, ns, sa)
}

func configuredPVCSourceFences(raw string) ([]pvccopy.ReviewedAdmissionFenceRef, bool, error) {
	if raw == "" {
		return nil, false, nil
	}
	if len(raw) > 64<<10 {
		return nil, true, fmt.Errorf("trusted source fence profile exceeds bounds")
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var refs []pvccopy.ReviewedAdmissionFenceRef
	if decoder.Decode(&refs) != nil || len(refs) == 0 || len(refs) > 32 {
		return nil, true, fmt.Errorf("trusted source fence profile invalid")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil, true, fmt.Errorf("trusted source fence profile trailing data")
	}
	return refs, true, nil
}
