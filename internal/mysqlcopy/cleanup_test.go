package mysqlcopy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// Pod polls block until BOTH waits have started. Serial delete/wait cleanup
// cannot pass; no real grace-period sleep is needed in this regression.
type cleanupCommands struct {
	*fakeKube
	mu         sync.Mutex
	deleted    map[string]string
	polled     map[string]bool
	both       chan struct{}
	earlyPoll  bool
	replacePod bool
}

func (k *cleanupCommands) Run(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	if args[0] == "delete" {
		var options map[string]any
		if err := json.NewDecoder(in).Decode(&options); err != nil {
			return err
		}
		if options["gracePeriodSeconds"] != float64(30) {
			return ErrUnsafe
		}
		parts := strings.Split(flag(args, "--raw"), "/")
		if len(parts) != 7 {
			return ErrUnsafe
		}
		k.mu.Lock()
		defer k.mu.Unlock()
		k.deleted[parts[6]] = str(options, "preconditions", "uid")
		return nil
	}
	if args[0] == "get" && args[1] == "pod" {
		k.mu.Lock()
		if len(k.deleted) != 2 {
			k.earlyPoll = true
		}
		k.polled[args[2]] = true
		if len(k.polled) == 2 {
			select {
			case <-k.both:
			default:
				close(k.both)
			}
		}
		replace := k.replacePod
		k.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-k.both:
		}
		if replace {
			return json.NewEncoder(out).Encode(object{"metadata": object{"uid": "foreign-replacement"}})
		}
		return nil
	}
	return k.fakeKube.Run(ctx, args, in, out)
}

func cleanupFixture() (*NativeDriver, *cleanupCommands, Plan) {
	p := testPlan()
	k := &cleanupCommands{fakeKube: initialKube(p), deleted: map[string]string{}, polled: map[string]bool{}, both: make(chan struct{})}
	d := NewProofDriver(DriverConfig{Commands: k})
	d.pods["source-owned-uid"] = ownedPod{endpoint: Endpoint{Pod: Ref{p.Source.PVC.Namespace, "source-helper", "source-owned-uid"}}}
	d.pods["target-owned-uid"] = ownedPod{endpoint: Endpoint{Pod: Ref{p.Target.Namespace, "target-helper", "target-owned-uid"}}, target: true}
	return d, k, p
}

func TestCleanupRequestsAllDeletesBeforeConcurrentWaits(t *testing.T) {
	d, k, p := cleanupFixture()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := d.Cleanup(ctx, p); err != nil {
		t.Fatal(err)
	}
	if k.earlyPoll || !reflect.DeepEqual(k.deleted, map[string]string{"source-helper": "source-owned-uid", "target-helper": "target-owned-uid"}) || len(k.polled) != 2 {
		t.Fatal("serialized or unpinned cleanup", k.deleted, k.polled)
	}
}

func TestCleanupRefusesReplacementPodAndNamespace(t *testing.T) {
	t.Run("pod", func(t *testing.T) {
		d, k, p := cleanupFixture()
		k.replacePod = true
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		err := d.Cleanup(ctx, p)
		if !errors.Is(err, ErrUnsafe) || len(FailureStages(err)) != 2 {
			t.Fatal("replacement accepted", err)
		}
	})
	t.Run("namespace", func(t *testing.T) {
		d, k, p := cleanupFixture()
		if err := d.pinNamespaces(context.Background(), p); err != nil {
			t.Fatal(err)
		}
		asMap(k.objects[key("namespace", "", p.Target.Namespace)]["metadata"])["uid"] = "recreated"
		err := d.Cleanup(context.Background(), p)
		if !errors.Is(err, ErrUnsafe) || StageOf(err) != CleanupNamespace || len(k.deleted) != 0 {
			t.Fatal("namespace replacement accepted", err)
		}
	})
}

type cleanupTransport struct {
	*memoryTransport
	cleanupErr     error
	parentCancel   context.CancelFunc
	cleanupHealthy bool
	cleanupBudget  time.Duration
}

func (m *cleanupTransport) Cleanup(ctx context.Context, _ Plan) error {
	m.cleanupHealthy = ctx.Err() == nil
	deadline, ok := ctx.Deadline()
	if ok {
		m.cleanupBudget = time.Until(deadline)
	}
	return m.cleanupErr
}
func (m *cleanupTransport) Commit(ctx context.Context, p Plan, r Receipt) error {
	if err := m.memoryTransport.Commit(ctx, p, r); err != nil {
		return err
	}
	if m.parentCancel != nil {
		m.parentCancel()
	}
	return nil
}
func TestCleanupIndependentBudgetAndFailureClearsReceipt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m := &cleanupTransport{memoryTransport: newMemory(), parentCancel: cancel, cleanupErr: context.DeadlineExceeded}
	r, err := (DomainExecutor{Transport: m, Authority: func(context.Context, Plan, Stage) error { return nil }}).ExecuteDomain(ctx, testPlan())
	if !m.committed || !m.cleanupHealthy || m.cleanupBudget < 44*time.Second || m.cleanupBudget > 45*time.Second {
		t.Fatal("cleanup lost independent bounded budget", m.cleanupBudget)
	}
	if r.PlanDigest != "" || !errors.Is(err, context.DeadlineExceeded) || StageOf(err) != CleanupHelpers {
		t.Fatal("cleanup failure became success", err)
	}
}
func TestTransferFailureSurvivesCleanupFailure(t *testing.T) {
	m := &cleanupTransport{memoryTransport: newMemory(), cleanupErr: ErrUnsafe}
	m.failDump = true
	r, err := (DomainExecutor{Transport: m, Authority: func(context.Context, Plan, Stage) error { return nil }}).ExecuteDomain(context.Background(), testPlan())
	if r.PlanDigest != "" || StageOf(err) != Transfer || !reflect.DeepEqual(FailureStages(err), []Stage{Transfer, CleanupHelpers}) {
		t.Fatal("primary failure lost", err)
	}
}
