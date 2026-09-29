package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/njoerd114/reminderrelay/internal/model"
	"github.com/njoerd114/reminderrelay/internal/state"
)

// Stats tracks the number of mutations performed in a single reconcile pass.
type Stats struct {
	Created   int
	Updated   int
	Deleted   int
	Conflicts int
	Errors    int
}

// Reconciler performs a single bidirectional sync pass across all configured
// list mappings. It is stateless between calls — all persistent state lives
// in the [StateStore].
//
// "Both exist and something changed" is resolved with a field-by-field merge
// (see [Reconciler.mergeAndSync]) rather than an item-level last-write-wins:
// each of the five syncable fields (title, description, due date, priority,
// completed) is attributed independently to whichever side changed it since
// the last sync, so an unrelated change on another side isn't silently
// discarded. Four of those fields are 2-way (Reminders vs HA-todo); the due
// date becomes a genuine 3-way field once calendar mirroring has created a
// mirrored event for an item, so it can also be attributed to an edit made
// directly on the HA calendar (see [mergeDueDate3]). A genuine conflict —
// more than one side changed the same field to different values — still
// needs a tie-break; see the "Conflict resolution" section in README.md for
// why that tie-break is only an approximation of true last-write-wins.
type Reconciler struct {
	rem   RemindersSource
	ha    HASource
	cal   CalendarSource
	store StateStore
	log   *slog.Logger
}

// NewReconciler creates a Reconciler wired to the given adapters and state
// store. ha must also implement [CalendarSource] for calendar mirroring to
// work — [homeassistant.Adapter] does; a value that only implements
// [HASource] disables calendar mirroring entirely (mirrorCalendar is skipped
// whenever cal is nil).
func NewReconciler(rem RemindersSource, ha HASource, store StateStore, logger *slog.Logger) *Reconciler {
	cal, _ := ha.(CalendarSource)
	return &Reconciler{rem: rem, ha: ha, cal: cal, store: store, log: logger}
}

// Run performs a full bidirectional sync for all list mappings. It returns
// aggregate statistics and the first error encountered (sync continues past
// individual item errors to maximise progress).
func (r *Reconciler) Run(ctx context.Context, listMappings map[string]model.ListMapping) (Stats, error) {
	var stats Stats
	var firstErr error

	listNames := make([]string, 0, len(listMappings))
	for name := range listMappings {
		listNames = append(listNames, name)
	}

	// 1. Fetch all Reminders items across configured lists.
	remItems, err := r.rem.FetchAll(ctx, listNames)
	if err != nil {
		return stats, fmt.Errorf("fetching reminders: %w", err)
	}

	// Index Reminders items by UID for fast lookup.
	remByUID := make(map[string]*model.Item, len(remItems))
	for _, item := range remItems {
		remByUID[item.UID] = item
	}

	// 2. Process each list mapping independently.
	for listName, mapping := range listMappings {
		ls, err := r.reconcileList(ctx, listName, mapping, remByUID)
		stats.Created += ls.Created
		stats.Updated += ls.Updated
		stats.Deleted += ls.Deleted
		stats.Conflicts += ls.Conflicts
		stats.Errors += ls.Errors
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	r.log.Info("reconcile complete",
		"created", stats.Created,
		"updated", stats.Updated,
		"deleted", stats.Deleted,
		"conflicts", stats.Conflicts,
		"errors", stats.Errors,
	)

	return stats, firstErr
}

// ReconcileEntity performs reconciliation for a single HA entity. Called by
// the WebSocket listener when a state_changed event is received.
func (r *Reconciler) ReconcileEntity(ctx context.Context, listName string, mapping model.ListMapping) (Stats, error) {
	// We need the Reminders items for just this list.
	remItems, err := r.rem.FetchAll(ctx, []string{listName})
	if err != nil {
		return Stats{}, fmt.Errorf("fetching reminders for %q: %w", listName, err)
	}

	remByUID := make(map[string]*model.Item, len(remItems))
	for _, item := range remItems {
		remByUID[item.UID] = item
	}

	return r.reconcileList(ctx, listName, mapping, remByUID)
}

// reconcileList performs bidirectional sync for a single list ↔ entity pair,
// then mirrors due-dated items to mapping.HACalendarEntity if configured.
func (r *Reconciler) reconcileList(ctx context.Context, listName string, mapping model.ListMapping, remByUID map[string]*model.Item) (Stats, error) {
	var stats Stats
	var firstErr error
	entityID := mapping.HAEntity

	r.log.Debug("reconciling list", "list", listName, "entity", entityID)

	// Fetch HA items for this entity.
	haItems, err := r.ha.GetItems(ctx, entityID)
	if err != nil {
		return stats, fmt.Errorf("fetching HA items for %s: %w", entityID, err)
	}

	// Index HA items by UID.
	haByUID := make(map[string]*model.Item, len(haItems))
	for i := range haItems {
		haItems[i].ListName = listName
		haByUID[haItems[i].UID] = &haItems[i]
	}

	// Fetch all tracked state items for this list.
	stateItems, err := r.store.GetAllItemsForList(ctx, listName)
	if err != nil {
		return stats, fmt.Errorf("fetching state items for %q: %w", listName, err)
	}

	// Fetch the current calendar events once per pass, if mirroring is
	// configured — this is the read half of the 3-way due-date merge (see
	// [Reconciler.mergeAndSync]). A listing failure degrades gracefully to
	// the 2-way Reminders/HA-only merge for this pass rather than failing
	// the whole list: calByUID stays nil, which every calByUID-aware call
	// below treats as "no calendar-side signal available this pass".
	var calByUID map[string]model.CalendarEvent
	if mapping.HACalendarEntity != "" && r.cal != nil {
		events, err := r.cal.ListCalendarEvents(ctx, mapping.HACalendarEntity)
		if err != nil {
			r.log.Warn("could not list calendar events, calendar due-date merge will fall back to 2-way this pass",
				"entity", mapping.HACalendarEntity, "error", err)
		} else {
			calByUID = make(map[string]model.CalendarEvent, len(events))
			for _, e := range events {
				calByUID[e.UID] = e
			}
		}
	}

	// Build a set of state RemindersUIDs and HAUIDs we've processed,
	// so we can detect new items after processing tracked ones.
	processedRemUIDs := make(map[string]bool, len(stateItems))
	processedHAUIDs := make(map[string]bool, len(stateItems))

	// Every state.Item touched this pass, tracked/created alike, so the
	// calendar mirror step below can run against the converged truth for
	// each one without a second read of Reminders/HA.
	var touched []*state.Item

	// 1. Process items we're already tracking.
	for _, si := range stateItems {
		remItem := remByUID[si.RemindersUID]
		haItem := haByUID[si.HAUID]

		if si.RemindersUID != "" {
			processedRemUIDs[si.RemindersUID] = true
		}
		if si.HAUID != "" {
			processedHAUIDs[si.HAUID] = true
		}

		switch {
		case remItem == nil && haItem == nil:
			// Both sides gone — just drop the tracked row.
			if err := r.store.DeleteItem(ctx, si.ID); err != nil {
				r.log.Error("cleaning up orphaned state row", "title", si.Title, "error", err)
				stats.Errors++
				if firstErr == nil {
					firstErr = err
				}
			}

		case remItem == nil:
			// Deleted from Reminders, still in HA → delete from HA.
			if err := r.ha.RemoveItem(ctx, entityID, haItem.UID, haItem.Title); err != nil {
				r.log.Error("deleting from HA", "title", si.Title, "error", err)
				stats.Errors++
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if err := r.deleteMirroredEvent(ctx, mapping, si); err != nil {
				r.log.Warn("deleting mirrored calendar event", "title", si.Title, "error", err)
			}
			if err := r.store.DeleteItem(ctx, si.ID); err != nil {
				r.log.Error("removing state row", "title", si.Title, "error", err)
			}
			stats.Deleted++

		case haItem == nil:
			// Deleted from HA, still in Reminders → delete from Reminders.
			if err := r.rem.Delete(ctx, remItem.UID); err != nil {
				r.log.Error("deleting from Reminders", "title", si.Title, "error", err)
				stats.Errors++
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if err := r.deleteMirroredEvent(ctx, mapping, si); err != nil {
				r.log.Warn("deleting mirrored calendar event", "title", si.Title, "error", err)
			}
			if err := r.store.DeleteItem(ctx, si.ID); err != nil {
				r.log.Error("removing state row", "title", si.Title, "error", err)
			}
			stats.Deleted++

		default:
			// A mirrored calendar event only becomes a merge participant
			// once it actually exists (si.CalendarEventUID set) — on the
			// very first pass that creates it, there's nothing there yet to
			// compare against, so this stays a 2-way merge until then (see
			// [Reconciler.mergeAndSync]).
			var calEvent *model.CalendarEvent
			calParticipates := calByUID != nil && si.CalendarEventUID != ""
			if calParticipates {
				if e, ok := calByUID[si.CalendarEventUID]; ok {
					calEvent = &e
				}
				// else: the event was deleted directly on the calendar —
				// calEvent stays nil, which the merge treats as "due date
				// cleared on the calendar side", same as any other field
				// clear.
			}
			calDueKey := ""
			if calEvent != nil {
				calDueKey = dueDateKey(calEvent.DueDate)
			}

			// Both exist — fast path: nothing changed since last sync on
			// any of the three sides.
			remHash := remItem.ContentHash()
			haHash := haItem.ContentHash()
			calUnchanged := !calParticipates || calDueKey == si.SyncedDueDate
			if remHash == si.LastSyncHash && haHash == si.LastSyncHash && calUnchanged {
				touched = append(touched, si)
				continue
			}

			// Capture the clocks BEFORE updating them: the merge below must
			// compare against how long each side's current (still-unsynced)
			// value has existed as of the START of this pass, not "now" —
			// "now" would make that side look newest on every single
			// first-detected change, which defeats the point of tracking it.
			haClock := si.HAModified
			r.trackHAObservedChange(si, haHash)
			calClock := si.CalendarModified
			if calParticipates {
				r.trackCalendarObservedChange(si, calDueKey)
			}

			var calDue *time.Time
			if calEvent != nil {
				calDue = calEvent.DueDate
			}
			changed, conflicted, err := r.mergeAndSync(ctx, si, remItem, haItem, calParticipates, calDue, haClock, calClock, entityID)
			if err != nil {
				r.log.Error("merge sync failed", "title", si.Title, "error", err)
				stats.Errors++
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if changed {
				stats.Updated++
			}
			if conflicted {
				stats.Conflicts++
				r.log.Info("conflict resolved", "title", si.Title)
			}
			touched = append(touched, si)
		}
	}

	// 2. Detect new Reminders items not in state DB → create in HA.
	for uid, remItem := range remByUID {
		if remItem.ListName != listName {
			continue
		}
		if processedRemUIDs[uid] {
			continue
		}

		r.log.Info("new reminder detected", "title", remItem.Title, "uid", uid)
		si, err := r.createInHA(ctx, remItem, entityID)
		if err != nil {
			r.log.Error("failed to create in HA", "title", remItem.Title, "error", err)
			stats.Errors++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		stats.Created++
		touched = append(touched, si)
	}

	// 3. Detect new HA items not in state DB → create in Reminders.
	for uid, haItem := range haByUID {
		if processedHAUIDs[uid] {
			continue
		}

		r.log.Info("new HA item detected", "title", haItem.Title, "uid", uid)
		si, err := r.createInReminders(ctx, haItem, entityID)
		if err != nil {
			r.log.Error("failed to create in Reminders", "title", haItem.Title, "error", err)
			stats.Errors++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		stats.Created++
		touched = append(touched, si)
	}

	// 4. Mirror due-dated items to the configured calendar entity, if any.
	// Calendar mirroring is a best-effort add-on to the core todo sync
	// above: a failure here (e.g. the calendar entity doesn't support
	// mutation) is logged and counted, but deliberately doesn't become the
	// pass's firstErr — it shouldn't make an otherwise-successful todo sync
	// report as failed.
	if mapping.HACalendarEntity != "" {
		for _, si := range touched {
			if err := r.mirrorCalendar(ctx, mapping.HACalendarEntity, si, calByUID); err != nil {
				r.log.Error("calendar mirror failed", "title", si.Title, "error", err)
				stats.Errors++
			}
		}
	}

	return stats, firstErr
}

// trackHAObservedChange stamps si.HAModified with the current time only when
// the HA-side content hash has moved on from what was observed in a
// previous pass — see the [state.Item.HAModified] doc comment for why this
// exists.
func (r *Reconciler) trackHAObservedChange(si *state.Item, haHash string) {
	if haHash != si.HALastSeenHash {
		si.HALastSeenHash = haHash
		si.HAModified = time.Now().UTC()
	}
}

// trackCalendarObservedChange is [Reconciler.trackHAObservedChange]'s
// counterpart for the calendar side's due date.
func (r *Reconciler) trackCalendarObservedChange(si *state.Item, calDueKey string) {
	if calDueKey != si.CalendarLastSeenDue {
		si.CalendarLastSeenDue = calDueKey
		si.CalendarModified = time.Now().UTC()
	}
}

// mergeAndSync resolves each of the five syncable fields independently
// against the last-synced snapshot in si — mergeField for
// title/description/priority/completed (always 2-way, Reminders vs HA-todo),
// and mergeDueDate/mergeDueDate3 for the due date (2-way, or 3-way once a
// mirrored calendar event exists — see calEvent below) — pushes the merged
// result to whichever side(s) didn't already have it, and persists the new
// snapshot. It returns whether anything was pushed and whether any single
// field hit a genuine conflict (more than one side changed it to different
// values).
//
// calParticipates is false when calendar mirroring isn't configured for
// this list, or when it is but no mirrored event exists for this item yet
// (the due date merge then stays 2-way). When true, calDue is the mirrored
// event's current due date — nil is a real value here (the due date was
// cleared, or the event itself was deleted; see the [dueDateKey] comparison
// in mergeDueDate3), not "no calendar signal at all" the way it would be
// when calParticipates is false. haClock/calClock are the caller-captured
// "as of the start of this pass" values of si.HAModified/si.CalendarModified
// — the approximated times each side's current content was first observed
// to differ, used only to break a genuine same-field conflict against
// remItem.ModifiedAt (EventKit's real timestamp). Pushing the resolved due
// date back onto the calendar itself isn't done here — it's a side effect
// of the ordinary calendar mirror step ([Reconciler.mirrorCalendar]) picking
// up si's updated SyncedDueDate after this returns.
func (r *Reconciler) mergeAndSync(ctx context.Context, si *state.Item, remItem, haItem *model.Item, calParticipates bool, calDue *time.Time, haClock, calClock time.Time, entityID string) (changed, conflicted bool, err error) {
	remClock := remItem.ModifiedAt

	title, remTitleDiff, haTitleDiff, titleConflict := mergeField(si.Title, remItem.Title, haItem.Title, remClock, haClock)
	desc, remDescDiff, haDescDiff, descConflict := mergeField(si.SyncedDescription, remItem.Description, haItem.Description, remClock, haClock)
	prio, remPrioDiff, haPrioDiff, prioConflict := mergeField(model.Priority(si.SyncedPriority), remItem.Priority, haItem.Priority, remClock, haClock)
	done, remDoneDiff, haDoneDiff, doneConflict := mergeField(si.SyncedCompleted, remItem.Completed, haItem.Completed, remClock, haClock)

	var due *time.Time
	var remDueDiff, haDueDiff, dueConflict bool
	if calParticipates {
		due, remDueDiff, haDueDiff, _, dueConflict = mergeDueDate3(
			si.SyncedDueDate, remItem.DueDate, haItem.DueDate, calDue,
			remClock, haClock, calClock,
		)
	} else {
		due, remDueDiff, haDueDiff, dueConflict = mergeDueDate(si.SyncedDueDate, remItem.DueDate, haItem.DueDate, remClock, haClock)
	}

	conflicted = titleConflict || descConflict || prioConflict || doneConflict || dueConflict

	merged := model.Item{
		UID:         remItem.UID,
		Title:       title,
		Description: desc,
		DueDate:     due,
		Priority:    prio,
		Completed:   done,
		ListName:    remItem.ListName,
	}

	remNeeds := remTitleDiff || remDescDiff || remPrioDiff || remDoneDiff || remDueDiff
	haNeeds := haTitleDiff || haDescDiff || haPrioDiff || haDoneDiff || haDueDiff

	if remNeeds {
		if err := r.rem.Update(ctx, remItem.UID, &merged); err != nil {
			return false, conflicted, fmt.Errorf("updating %q in Reminders: %w", merged.Title, err)
		}
	}
	if haNeeds {
		if err := r.ha.UpdateItem(ctx, entityID, haItem.UID, haItem.Title, &merged); err != nil {
			return false, conflicted, fmt.Errorf("updating %q in HA: %w", merged.Title, err)
		}
	}

	si.Title = merged.Title
	si.SyncedDescription = merged.Description
	si.SyncedDueDate = dueDateKey(merged.DueDate)
	si.SyncedPriority = int(merged.Priority)
	si.SyncedCompleted = merged.Completed
	si.LastSyncHash = merged.ContentHash()
	si.RemindersModified = remItem.ModifiedAt
	si.LastSyncedAt = time.Now().UTC()

	if err := r.store.UpsertItem(ctx, si); err != nil {
		return remNeeds || haNeeds, conflicted, fmt.Errorf("persisting merged state for %q: %w", merged.Title, err)
	}
	return remNeeds || haNeeds, conflicted, nil
}

// mergeField resolves a single comparable field given the last-synced value,
// the current value on each side, and each side's "changed at" clock for
// tie-breaking. remDiffers/haDiffers report whether that side needs to
// receive the resolved value to converge; conflicted reports whether this
// was a genuine same-field conflict (both sides changed it, to different
// values) rather than a clean one-sided change.
func mergeField[T comparable](synced, remValue, haValue T, remClock, haClock time.Time) (resolved T, remDiffers, haDiffers, conflicted bool) {
	remChanged := remValue != synced
	haChanged := haValue != synced

	switch {
	case !remChanged && !haChanged:
		resolved = synced
	case remChanged && !haChanged:
		resolved = remValue
	case !remChanged && haChanged:
		resolved = haValue
	default:
		conflicted = remValue != haValue
		if !conflicted {
			resolved = remValue // same new value on both sides
		} else if !remClock.Before(haClock) {
			resolved = remValue // ties favour Reminders, the primary source
		} else {
			resolved = haValue
		}
	}

	remDiffers = resolved != remValue
	haDiffers = resolved != haValue
	return resolved, remDiffers, haDiffers, conflicted
}

// mergeDueDate is [mergeField]'s counterpart for *time.Time, which isn't
// safely `comparable` (see dueDateKey) and needs nil handling.
func mergeDueDate(syncedKey string, remDue, haDue *time.Time, remClock, haClock time.Time) (resolved *time.Time, remDiffers, haDiffers, conflicted bool) {
	remKey := dueDateKey(remDue)
	haKey := dueDateKey(haDue)
	remChanged := remKey != syncedKey
	haChanged := haKey != syncedKey

	switch {
	case !remChanged && !haChanged:
		resolved = remDue
	case remChanged && !haChanged:
		resolved = remDue
	case !remChanged && haChanged:
		resolved = haDue
	default:
		conflicted = remKey != haKey
		if !conflicted {
			resolved = remDue
		} else if !remClock.Before(haClock) {
			resolved = remDue
		} else {
			resolved = haDue
		}
	}

	resolvedKey := dueDateKey(resolved)
	remDiffers = resolvedKey != remKey
	haDiffers = resolvedKey != haKey
	return resolved, remDiffers, haDiffers, conflicted
}

// mergeDueDate3 is [mergeDueDate]'s 3-way counterpart, used once a mirrored
// calendar event exists for this item — the one field the calendar mirror
// is genuinely bidirectional for (see README.md's "Calendar Mirroring"
// section). calDue is nil either because the due date was genuinely cleared
// on the calendar, or because the mirrored event itself was deleted; both
// are treated identically as "the calendar side no longer has this due
// date". On a same-value-but-different-clock 3-way tie (more than one
// source changed to different values, with equal clocks), precedence goes
// Reminders, then HA-todo, then calendar — the same "ties favour Reminders"
// rule [mergeField] uses, extended with calendar as the lowest-precedence
// addition.
func mergeDueDate3(syncedKey string, remDue, haDue, calDue *time.Time, remClock, haClock, calClock time.Time) (resolved *time.Time, remDiffers, haDiffers, calDiffers, conflicted bool) {
	type candidate struct {
		key   string
		value *time.Time
		clock time.Time
	}
	candidates := []candidate{
		{dueDateKey(remDue), remDue, remClock},
		{dueDateKey(haDue), haDue, haClock},
		{dueDateKey(calDue), calDue, calClock},
	}

	var changed []candidate
	for _, c := range candidates {
		if c.key != syncedKey {
			changed = append(changed, c)
		}
	}

	winner := candidate{key: syncedKey, value: nil}
	if syncedKey != "" {
		// synced key came from whichever side wasn't nil last time; any
		// unchanged candidate carries the same value, so pick one.
		for _, c := range candidates {
			if c.key == syncedKey {
				winner = c
				break
			}
		}
	}

	switch len(changed) {
	case 0:
		// winner already set above (the synced value).
	case 1:
		winner = changed[0]
	default:
		allSame := true
		for _, c := range changed[1:] {
			if c.key != changed[0].key {
				allSame = false
				break
			}
		}
		if allSame {
			winner = changed[0]
		} else {
			conflicted = true
			winner = changed[0]
			for _, c := range changed[1:] {
				if c.clock.After(winner.clock) {
					winner = c
				}
			}
		}
	}

	remDiffers = winner.key != candidates[0].key
	haDiffers = winner.key != candidates[1].key
	calDiffers = winner.key != candidates[2].key
	return winner.value, remDiffers, haDiffers, calDiffers, conflicted
}

// dueDateKey normalises a due date to a comparable string: RFC3339 in UTC,
// or "" for no due date. Used instead of comparing *time.Time directly,
// since pointer identity isn't value equality and time.Time's own == can
// give false negatives on values carrying a monotonic reading.
func dueDateKey(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// createInHA pushes a new Reminders item to HA and writes the state DB entry.
func (r *Reconciler) createInHA(ctx context.Context, remItem *model.Item, entityID string) (*state.Item, error) {
	if err := r.ha.AddItem(ctx, entityID, remItem); err != nil {
		return nil, fmt.Errorf("adding %q to HA: %w", remItem.Title, err)
	}

	// After adding, fetch items again to get the HA UID.
	haItems, err := r.ha.GetItems(ctx, entityID)
	if err != nil {
		return nil, fmt.Errorf("refetching items from %s: %w", entityID, err)
	}

	var haUID string
	for _, h := range haItems {
		if h.Title == remItem.Title {
			haUID = h.UID
			break
		}
	}

	now := time.Now().UTC()
	si := &state.Item{
		RemindersUID:      remItem.UID,
		HAUID:             haUID,
		ListName:          remItem.ListName,
		Title:             remItem.Title,
		SyncedDescription: remItem.Description,
		SyncedDueDate:     dueDateKey(remItem.DueDate),
		SyncedPriority:    int(remItem.Priority),
		SyncedCompleted:   remItem.Completed,
		LastSyncHash:      remItem.ContentHash(),
		HALastSeenHash:    remItem.ContentHash(),
		RemindersModified: remItem.ModifiedAt,
		LastSyncedAt:      now,
	}
	if err := r.store.UpsertItem(ctx, si); err != nil {
		return nil, err
	}
	return si, nil
}

// createInReminders pushes a new HA item to Reminders and writes the state DB entry.
func (r *Reconciler) createInReminders(ctx context.Context, haItem *model.Item, entityID string) (*state.Item, error) {
	uid, err := r.rem.Create(ctx, haItem)
	if err != nil {
		return nil, fmt.Errorf("creating %q in Reminders: %w", haItem.Title, err)
	}

	now := time.Now().UTC()
	si := &state.Item{
		RemindersUID:      uid,
		HAUID:             haItem.UID,
		ListName:          haItem.ListName,
		Title:             haItem.Title,
		SyncedDescription: haItem.Description,
		SyncedDueDate:     dueDateKey(haItem.DueDate),
		SyncedPriority:    int(haItem.Priority),
		SyncedCompleted:   haItem.Completed,
		LastSyncHash:      haItem.ContentHash(),
		HALastSeenHash:    haItem.ContentHash(),
		LastSyncedAt:      now,
	}
	if err := r.store.UpsertItem(ctx, si); err != nil {
		return nil, err
	}
	return si, nil
}

// --- Calendar mirroring -------------------------------------------------

// mirrorCalendar keeps si's mirrored event on calendarEntityID in sync with
// its currently-synced due date and completion state (title and completed
// status flow one-way here; the due date itself may have arrived from the
// calendar side via the 3-way merge in [Reconciler.mergeAndSync], in which
// case this is often a no-op — see the CalendarSyncHash check below). If the
// entity doesn't declare full create+update+delete support, mirroring is
// skipped for this item with a logged error rather than failing the whole
// reconcile pass.
//
// calByUID is this pass's calendar listing (nil if it couldn't be fetched),
// used to tell a genuinely-missing event apart from one that just needs
// updating, instead of discovering that by making a doomed API call first.
func (r *Reconciler) mirrorCalendar(ctx context.Context, calendarEntityID string, si *state.Item, calByUID map[string]model.CalendarEvent) error {
	if r.cal == nil {
		return nil
	}

	capable, err := r.cal.CalendarSupportsMutation(ctx, calendarEntityID)
	if err != nil {
		return fmt.Errorf("checking calendar capabilities for %s: %w", calendarEntityID, err)
	}
	if !capable {
		return fmt.Errorf("calendar %s does not support create+update+delete events — see README.md's calendar mirroring section", calendarEntityID)
	}

	// nil calByUID (this pass's listing failed) means "unknown" — assume it
	// still exists, preserving the pre-3-way behaviour of just attempting
	// the call and recovering from a not-found error if that guess is wrong.
	eventStillExists := true
	if calByUID != nil && si.CalendarEventUID != "" {
		_, eventStillExists = calByUID[si.CalendarEventUID]
	}

	hasDue := si.SyncedDueDate != ""

	if !hasDue {
		if si.CalendarEventUID == "" {
			return nil // nothing mirrored, nothing to do
		}
		if eventStillExists {
			if err := r.cal.DeleteCalendarEvent(ctx, calendarEntityID, si.CalendarEventUID); err != nil {
				return fmt.Errorf("deleting mirrored event for %q: %w", si.Title, err)
			}
		}
		// Already gone (most likely: the due date was cleared *because* the
		// user deleted the event on the calendar, and the 3-way merge
		// picked that up) — just drop our own reference to it.
		si.CalendarEventUID = ""
		si.CalendarSyncHash = ""
		si.CalendarLastSeenDue = ""
		return r.store.UpsertItem(ctx, si)
	}

	due, err := time.Parse(time.RFC3339, si.SyncedDueDate)
	if err != nil {
		return fmt.Errorf("parsing synced due date for %q: %w", si.Title, err)
	}
	mirrorItem := &model.Item{Title: si.Title, DueDate: &due, Completed: si.SyncedCompleted}
	hash := mirrorItem.CalendarHash()

	if hash == si.CalendarSyncHash && si.CalendarEventUID != "" && eventStillExists {
		return nil // already mirrored and unchanged
	}

	if si.CalendarEventUID == "" || !eventStillExists {
		uid, err := r.cal.CreateCalendarEvent(ctx, calendarEntityID, mirrorItem)
		if err != nil {
			return fmt.Errorf("creating mirrored event for %q: %w", si.Title, err)
		}
		si.CalendarEventUID = uid
	} else if err := r.cal.UpdateCalendarEvent(ctx, calendarEntityID, si.CalendarEventUID, mirrorItem); err != nil {
		// Our cache said it existed but the update failed anyway (a race
		// with a deletion that happened after we listed events this pass);
		// recreate it rather than leaving the mirror permanently broken.
		r.log.Warn("updating mirrored calendar event failed, recreating", "title", si.Title, "uid", si.CalendarEventUID, "error", err)
		uid, cerr := r.cal.CreateCalendarEvent(ctx, calendarEntityID, mirrorItem)
		if cerr != nil {
			return fmt.Errorf("recreating mirrored event for %q: %w", si.Title, cerr)
		}
		si.CalendarEventUID = uid
	}

	si.CalendarSyncHash = hash
	si.CalendarLastSeenDue = dueDateKey(mirrorItem.DueDate)
	return r.store.UpsertItem(ctx, si)
}

// deleteMirroredEvent removes si's mirrored calendar event, if any, when the
// underlying todo item itself is deleted from both sides.
func (r *Reconciler) deleteMirroredEvent(ctx context.Context, mapping model.ListMapping, si *state.Item) error {
	if r.cal == nil || mapping.HACalendarEntity == "" || si.CalendarEventUID == "" {
		return nil
	}
	return r.cal.DeleteCalendarEvent(ctx, mapping.HACalendarEntity, si.CalendarEventUID)
}
