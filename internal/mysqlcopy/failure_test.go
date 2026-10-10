package mysqlcopy

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestFailureDiagnosticsNeverExposeErrorBodies(t *testing.T) {
	raw := errors.New("password=secret SQL INSERT INTO private VALUES ('sensitive')")
	err := safeFailure(Transfer, errors.Join(raw, safeFailure(WaitTargetHelperDeleted, context.DeadlineExceeded)))
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "SQL") || strings.Contains(err.Error(), "sensitive") {
		t.Fatal("raw body leaked")
	}
	stage, code := FailureInfo(fmt.Errorf("raw wrapper SQL: %w", err))
	if stage != Transfer || code != "operation_failed" || StageOf(err) != Transfer || !reflect.DeepEqual(FailureStages(err), []Stage{Transfer, WaitTargetHelperDeleted}) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("unsafe or lost diagnostics", stage, code)
	}
	if StageOf(raw) != UnknownFailure || len(FailureStages(raw)) != 0 {
		t.Fatal("raw text interpreted as stage")
	}
	if stage, code := FailureInfo(nil); stage != "" || code != "" {
		t.Fatal("nil failure")
	}
}

type cyclicFailure struct{}

func (e *cyclicFailure) Error() string { return "secret" }
func (e *cyclicFailure) Unwrap() error { return e }
func TestFailureStagesBoundedAndDefinedOnly(t *testing.T) {
	if stage, code := FailureInfo(&cyclicFailure{}); stage != UnknownFailure || code != "operation_failed" {
		t.Fatal(stage, code)
	}
	if got := FailureStages(&cyclicFailure{}); len(got) != 0 {
		t.Fatal(got)
	}
	err := errors.Join(&Failure{stage: "password=secret", code: "secret"}, safeFailure(CommitReceipt, ErrUnsafe))
	if got := FailureStages(err); !reflect.DeepEqual(got, []Stage{CommitReceipt}) {
		t.Fatal(got)
	}
}

func TestNestedTeardownErrorsKeepPrimaryFirst(t *testing.T) {
	err := errors.Join(errors.Join(safeFailure(Transfer, ErrOperation), safeFailure(ReleaseDDL, ErrUnsafe)), safeFailure(CleanupHelpers, context.DeadlineExceeded))
	stage, code := FailureInfo(err)
	if stage != Transfer || code != "operation_failed" || StageOf(err) != Transfer || !reflect.DeepEqual(FailureStages(err), []Stage{Transfer, ReleaseDDL, CleanupHelpers}) {
		t.Fatal("nested teardown masked primary", stage, code, FailureStages(err))
	}
}
