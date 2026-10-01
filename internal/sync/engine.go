package sync

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/trace"

	"github.com/njoerd114/reminderrelay/internal/model"
)

const (
	otelScope       = "reminderrelay/sync"
	spanReconcile   = "sync.reconcile"
	metricCreated   = "reminderrelay.sync.items.created"
	metricUpdated   = "reminderrelay.sync.items.updated"
	metricDeleted   = "reminderrelay.sync.items.deleted"
	metricConflicts = "reminderrelay.sync.conflicts"
	metricErrors    = "reminderrelay.sync.errors"

	// wsDebounceWindow is how long the engine waits after a WebSocket
	// state_changed event before reconciling that entity. HA tends to fire
	// several events in quick succession for a single user action (e.g. one
	// per field update); without debouncing, each would trigger its own
	// reconcile pass.
	wsDebounceWindow = 2 * time.Second
)

// HAConnector provides WebSocket lifecycle methods for the Engine.
// Implemented by [homeassistant.Adapter].
type HAConnector interface {
	HASource
	Connect(ctx context.Context) error
	Close() error
	SubscribeChanges(ctx context.Context, entityIDs []string, callback func(entityID string)) error
}

// Engine orchestrates the sync lifecycle: polling loop + optional WebSocket
// listener for instant HA updates. Create one with [NewEngine] and start it
// with [Engine.Run].
type Engine struct {
	reconciler   *Reconciler
	haConn       HAConnector
	listMappings map[string]model.ListMapping
	pollInterval time.Duration
	log          *slog.Logger

	// needsWS is true when at least one list mapping has a calendar entity
	// configured — calendar event update/delete are WebSocket-only (see
	// [homeassistant.Adapter.UpdateCalendarEvent]), so RunOnce needs to
	// connect the WebSocket even outside daemon mode, where it's normally
	// only opened for SubscribeChanges.
	needsWS bool

	// debounceWindow is how long a WS event waits before triggering a
	// reconcile; overridable in tests. debounceTimers holds one pending
	// timer per entity ID.
	debounceWindow time.Duration
	debounceMu     sync.Mutex
	debounceTimers map[string]*time.Timer

	// OTel instruments — always non-nil (no-op when telemetry is disabled).
	tracer       trace.Tracer
	cntCreated   metric.Int64Counter
	cntUpdated   metric.Int64Counter
	cntDeleted   metric.Int64Counter
	cntConflicts metric.Int64Counter
	cntErrors    metric.Int64Counter
}

// NewEngine creates an Engine. If haConn is nil, WebSocket subscriptions are
// skipped and the engine runs polling-only.
func NewEngine(reconciler *Reconciler, haConn HAConnector, listMappings map[string]model.ListMapping, pollInterval time.Duration, logger *slog.Logger) *Engine {
	tracer := otel.Tracer(otelScope)
	meter := otel.Meter(otelScope)

	mustCounter := func(name, desc string) metric.Int64Counter {
		c, err := meter.Int64Counter(name, metric.WithDescription(desc))
		if err != nil {
			logger.Error("creating OTel counter", "name", name, "error", err)
			return noop.Int64Counter{}
		}
		return c
	}

	needsWS := false
	for _, m := range listMappings {
		if m.HACalendarEntity != "" {
			needsWS = true
			break
		}
	}

	return &Engine{
		reconciler:   reconciler,
		haConn:       haConn,
		listMappings: listMappings,
		pollInterval: pollInterval,
		needsWS:      needsWS,
		log:          logger,

		debounceWindow: wsDebounceWindow,
		debounceTimers: make(map[string]*time.Timer),

		tracer:       tracer,
		cntCreated:   mustCounter(metricCreated, "Number of items created during sync"),
		cntUpdated:   mustCounter(metricUpdated, "Number of items updated during sync"),
		cntDeleted:   mustCounter(metricDeleted, "Number of items deleted during sync"),
		cntConflicts: mustCounter(metricConflicts, "Number of conflict resolutions during sync"),
		cntErrors:    mustCounter(metricErrors, "Number of errors encountered during sync"),
	}
}

// reconcile runs one full reconcile pass, recording a trace span and metrics.
func (e *Engine) reconcile(ctx context.Context) (Stats, error) {
	ctx, span := e.tracer.Start(ctx, spanReconcile)
	defer span.End()

	// Calendar mirroring needs the WebSocket connection (event update/delete
	// aren't REST services). Connect is idempotent, so this is a no-op once
	// SubscribeChanges (daemon mode) has already connected it.
	if e.needsWS && e.haConn != nil {
		if err := e.haConn.Connect(ctx); err != nil {
			e.log.Warn("could not connect WebSocket for calendar mirroring, mirrored due dates will not update this pass", "error", err)
		}
	}

	stats, err := e.reconciler.Run(ctx, e.listMappings)

	// Record counters — these are always safe even if the span is a no-op.
	if stats.Created > 0 {
		e.cntCreated.Add(ctx, int64(stats.Created))
	}
	if stats.Updated > 0 {
		e.cntUpdated.Add(ctx, int64(stats.Updated))
	}
	if stats.Deleted > 0 {
		e.cntDeleted.Add(ctx, int64(stats.Deleted))
	}
	if stats.Conflicts > 0 {
		e.cntConflicts.Add(ctx, int64(stats.Conflicts))
	}
	if stats.Errors > 0 {
		e.cntErrors.Add(ctx, int64(stats.Errors))
	}

	span.SetAttributes(
		attribute.Int("sync.created", stats.Created),
		attribute.Int("sync.updated", stats.Updated),
		attribute.Int("sync.deleted", stats.Deleted),
		attribute.Int("sync.conflicts", stats.Conflicts),
		attribute.Int("sync.errors", stats.Errors),
	)
	if err != nil {
		span.RecordError(err)
	}
	return stats, err
}

// debounceReconcile schedules a ReconcileEntity call for entityID after
// e.debounceWindow, resetting any pending timer for that entity. A burst of
// WS events for the same entity (HA commonly fires several per user action)
// thus triggers a single reconcile once things settle, rather than one per
// event.
func (e *Engine) debounceReconcile(ctx context.Context, listName, entityID string) {
	e.debounceMu.Lock()
	defer e.debounceMu.Unlock()

	if t, ok := e.debounceTimers[entityID]; ok {
		t.Stop()
	}
	e.debounceTimers[entityID] = time.AfterFunc(e.debounceWindow, func() {
		if ctx.Err() != nil {
			return
		}
		e.log.Info("debounced WS event triggered reconcile", "entity_id", entityID)
		if _, err := e.reconciler.ReconcileEntity(ctx, listName, e.listMappings[listName]); err != nil {
			e.log.Error("WS-triggered reconcile failed", "entity_id", entityID, "error", err)
		}
	})
}

// RunOnce performs a single reconciliation pass and returns.
func (e *Engine) RunOnce(ctx context.Context) (Stats, error) {
	return e.reconcile(ctx)
}

// Run starts the polling loop and optional WebSocket listener. It blocks until
// ctx is cancelled.
func (e *Engine) Run(ctx context.Context) error {
	// Start WS listener if available.
	if e.haConn != nil {
		if err := e.haConn.Connect(ctx); err != nil {
			e.log.Error("WebSocket connection failed, falling back to polling-only", "error", err)
		} else {
			defer func() { _ = e.haConn.Close() }()

			entityIDs := make([]string, 0, len(e.listMappings))
			for _, m := range e.listMappings {
				entityIDs = append(entityIDs, m.HAEntity)
			}

			// Build reverse mapping: entityID → (listName, mapping).
			entityToList := make(map[string]string, len(e.listMappings))
			for listName, m := range e.listMappings {
				entityToList[m.HAEntity] = listName
			}

			go func() {
				err := e.haConn.SubscribeChanges(ctx, entityIDs, func(entityID string) {
					listName, ok := entityToList[entityID]
					if !ok {
						return
					}
					e.debounceReconcile(ctx, listName, entityID)
				})
				if err != nil && ctx.Err() == nil {
					e.log.Error("WS subscription ended unexpectedly", "error", err)
				}
			}()
		}
	}

	// Polling loop.
	ticker := time.NewTicker(e.pollInterval)
	defer ticker.Stop()

	// Run an immediate first pass.
	if _, err := e.reconcile(ctx); err != nil {
		e.log.Error("initial reconcile failed", "error", err)
	}

	for {
		select {
		case <-ctx.Done():
			e.log.Info("sync engine shutting down")
			return ctx.Err()
		case <-ticker.C:
			if _, err := e.reconcile(ctx); err != nil {
				e.log.Error("reconcile failed", "error", err)
			}
		}
	}
}
