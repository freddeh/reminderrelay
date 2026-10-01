package sync

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/njoerd114/reminderrelay/internal/model"
)

// blockingReminders wraps a RemindersSource and blocks the first call to
// FetchAll until told to proceed, letting tests deterministically interleave
// two reconcile passes.
type blockingReminders struct {
	RemindersSource
	started chan struct{}
	proceed chan struct{}
	once    sync.Once
}

func (b *blockingReminders) FetchAll(ctx context.Context, listNames []string) ([]*model.Item, error) {
	b.once.Do(func() {
		close(b.started)
		<-b.proceed
	})
	return b.RemindersSource.FetchAll(ctx, listNames)
}

// TestReconciler_RunAndReconcileEntity_AreMutuallyExclusive guards against the
// duplicate-creation race where the polling loop (Run) and a WS-triggered
// reconcile (ReconcileEntity) observe the same untracked item concurrently
// and both create it on the other side. The Reconciler's mutex must make
// ReconcileEntity wait until Run has released it.
func TestReconciler_RunAndReconcileEntity_AreMutuallyExclusive(t *testing.T) {
	now := time.Now().UTC()
	remItem := newItem("rem-1", "Buy milk", "Shopping", model.PriorityNone, false, now)

	blocking := &blockingReminders{
		RemindersSource: newMockReminders(remItem),
		started:         make(chan struct{}),
		proceed:         make(chan struct{}),
	}
	ha := newMockHA()
	store := newMockStore()

	r := NewReconciler(blocking, ha, store, testLogger)

	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_, _ = r.Run(context.Background(), testMappings)
	}()

	select {
	case <-blocking.started:
	case <-time.After(2 * time.Second):
		t.Fatal("Run never reached FetchAll")
	}
	// Run is now inside FetchAll, holding r.mu.

	reconcileDone := make(chan struct{})
	go func() {
		defer close(reconcileDone)
		_, _ = r.ReconcileEntity(context.Background(), "Shopping", testMappings["Shopping"])
	}()

	select {
	case <-reconcileDone:
		t.Fatal("ReconcileEntity completed while Run was still in progress; Reconciler is not serialized")
	case <-time.After(100 * time.Millisecond):
		// Expected: ReconcileEntity is blocked waiting for r.mu.
	}

	close(blocking.proceed) // let Run finish

	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run never finished after being unblocked")
	}

	select {
	case <-reconcileDone:
	case <-time.After(2 * time.Second):
		t.Fatal("ReconcileEntity never completed after Run finished")
	}

	// Exactly one state entry should exist — no duplicate creation from the
	// interleaved passes.
	if store.count() != 1 {
		t.Errorf("state items = %d, want 1 (no duplicate from concurrent reconcile)", store.count())
	}
}
