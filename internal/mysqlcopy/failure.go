package mysqlcopy

import (
	"context"
	"errors"
)

// Diagnostics expose only a fixed phase and classification. They retain no
// underlying kubectl/MySQL error, SQL, object name, password or output body.
type Failure struct {
	stage Stage
	code  string
	cause error
}

func (f *Failure) Error() string {
	return "mysql copy failed [stage=" + string(f.stage) + " code=" + f.code + "]"
}
func (f *Failure) Unwrap() error { return f.cause }

const (
	UnknownFailure          Stage = "unknown"
	ValidatePlan            Stage = "validate_plan"
	CheckCredentials        Stage = "check_credentials"
	InspectTarget           Stage = "inspect_target"
	ShutdownTarget          Stage = "shutdown_target"
	CommitReceipt           Stage = "commit_receipt"
	ReleaseDDL              Stage = "release_ddl"
	CleanupHelpers          Stage = "cleanup_helpers"
	CleanupNamespace        Stage = "cleanup_namespace"
	DeleteSourceHelper      Stage = "delete_source_helper"
	WaitSourceHelperDeleted Stage = "wait_source_helper_deleted"
	DeleteTargetHelper      Stage = "delete_target_helper"
	WaitTargetHelperDeleted Stage = "wait_target_helper_deleted"
)

func definedStage(stage Stage) bool {
	switch stage {
	case ReadSource, HoldDDL, ProvisionTarget, Transfer, Verify, Complete, ValidatePlan, CheckCredentials, InspectTarget, ShutdownTarget, CommitReceipt, ReleaseDDL, CleanupHelpers, CleanupNamespace, DeleteSourceHelper, WaitSourceHelperDeleted, DeleteTargetHelper, WaitTargetHelperDeleted, UnknownFailure:
		return true
	}
	return false
}

func safeFailure(stage Stage, err error) error {
	if err == nil {
		return nil
	}
	if group, ok := err.(interface{ Unwrap() []error }); ok {
		var failures []error
		for _, child := range group.Unwrap() {
			failures = append(failures, safeFailure(stage, child))
		}
		return errors.Join(failures...)
	}
	var f *Failure
	if errors.As(err, &f) && f.stage != "" && f.code != "" {
		return &Failure{f.stage, f.code, safeCause(f.cause)}
	}
	if !definedStage(stage) {
		stage = UnknownFailure
	}
	code := "operation_failed"
	cause := ErrOperation
	switch {
	case errors.Is(err, context.Canceled):
		code = "cancelled"
		cause = context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		code = "deadline_exceeded"
		cause = context.DeadlineExceeded
	case errors.Is(err, ErrPartial):
		code = "partial_target"
		cause = ErrPartial
	case errors.Is(err, ErrUnsafe):
		code = "safety_refusal"
		cause = ErrUnsafe
	}
	return &Failure{stage, code, cause}
}
func safeCause(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, ErrPartial):
		return ErrPartial
	case errors.Is(err, ErrUnsafe):
		return ErrUnsafe
	default:
		return ErrOperation
	}
}

// FailureInfo returns the primary constant phase/code, including when a
// secondary cleanup/release failure is joined. No parsing of error text needed.
func FailureInfo(err error) (Stage, string) {
	queue := []error{err}
	for visited := 0; len(queue) > 0 && visited < 64; visited++ {
		e := queue[0]
		queue = queue[1:]
		if e == nil {
			continue
		}
		if f, ok := e.(*Failure); ok {
			stage := f.stage
			if !definedStage(stage) {
				stage = UnknownFailure
			}
			code := f.code
			switch code {
			case "cancelled", "deadline_exceeded", "partial_target", "safety_refusal", "operation_failed":
			default:
				code = "operation_failed"
			}
			return stage, code
		}
		switch w := e.(type) {
		case interface{ Unwrap() []error }:
			children := w.Unwrap()
			if len(children) > 64 {
				children = children[:64]
			}
			queue = append(children, queue...)
		case interface{ Unwrap() error }:
			queue = append([]error{w.Unwrap()}, queue...)
		}
	}
	if err == nil {
		return "", ""
	}
	return "unknown", "operation_failed"
}

// FailureStages walks at most 64 error nodes and returns at most 20 distinct
// defined constants. Joined teardown failures do not erase the primary stage.
// It never examines raw error text or accepts caller-supplied stage strings.
func FailureStages(err error) []Stage {
	queue := []error{err}
	seen := map[Stage]bool{}
	var stages []Stage
	for visited := 0; len(queue) > 0 && visited < 64 && len(stages) < 20; visited++ {
		e := queue[0]
		queue = queue[1:]
		if e == nil {
			continue
		}
		if f, ok := e.(*Failure); ok {
			if definedStage(f.stage) && !seen[f.stage] {
				seen[f.stage] = true
				stages = append(stages, f.stage)
			}
		}
		switch w := e.(type) {
		case interface{ Unwrap() []error }:
			children := w.Unwrap()
			if len(children) > 64 {
				children = children[:64]
			}
			queue = append(children, queue...)
		case interface{ Unwrap() error }:
			queue = append([]error{w.Unwrap()}, queue...)
		}
	}
	return stages
}

// StageOf is the primary defined diagnostic phase, never a raw error string.
func StageOf(err error) Stage {
	stages := FailureStages(err)
	if len(stages) > 0 {
		return stages[0]
	}
	if err == nil {
		return ""
	}
	return UnknownFailure
}
