package sync

import (
	"context"
	"fmt"
	"sync"

	"github.com/njoerd114/reminderrelay/internal/model"
	"github.com/njoerd114/reminderrelay/internal/state"
)

// --- Mock Reminders Source ---------------------------------------------------

type mockReminders struct {
	mu      sync.Mutex
	items   map[string]*model.Item // UID → Item
	nextUID int
}

func newMockReminders(items ...*model.Item) *mockReminders {
	m := &mockReminders{items: make(map[string]*model.Item), nextUID: len(items)}
	for _, item := range items {
		m.items[item.UID] = item
	}
	return m
}

func (m *mockReminders) FetchAll(_ context.Context, listNames []string) ([]*model.Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	nameSet := make(map[string]bool, len(listNames))
	for _, n := range listNames {
		nameSet[n] = true
	}

	var result []*model.Item
	for _, item := range m.items {
		if nameSet[item.ListName] {
			result = append(result, item)
		}
	}
	return result, nil
}

func (m *mockReminders) Create(_ context.Context, item *model.Item) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.nextUID++
	uid := fmt.Sprintf("rem-%d", m.nextUID)
	cp := *item
	cp.UID = uid
	m.items[uid] = &cp
	return uid, nil
}

func (m *mockReminders) Update(_ context.Context, uid string, item *model.Item) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	existing, ok := m.items[uid]
	if !ok {
		return fmt.Errorf("reminder %q not found", uid)
	}
	existing.Title = item.Title
	existing.Description = item.Description
	existing.DueDate = item.DueDate
	existing.Priority = item.Priority
	existing.Completed = item.Completed
	existing.ModifiedAt = item.ModifiedAt
	return nil
}

func (m *mockReminders) Delete(_ context.Context, uid string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.items[uid]; !ok {
		return fmt.Errorf("reminder %q not found", uid)
	}
	delete(m.items, uid)
	return nil
}

func (m *mockReminders) get(uid string) *model.Item {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.items[uid]
}

func (m *mockReminders) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.items)
}

// --- Mock HA Source -----------------------------------------------------------

type mockHA struct {
	mu      sync.Mutex
	items   map[string][]model.Item // entityID → items
	nextUID int
}

func newMockHA() *mockHA {
	return &mockHA{items: make(map[string][]model.Item), nextUID: 100}
}

func (m *mockHA) addItems(entityID string, items ...model.Item) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[entityID] = append(m.items[entityID], items...)
}

func (m *mockHA) GetItems(_ context.Context, entityID string) ([]model.Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	items := m.items[entityID]
	// Return copies.
	result := make([]model.Item, len(items))
	copy(result, items)
	return result, nil
}

func (m *mockHA) AddItem(_ context.Context, entityID string, item *model.Item) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.nextUID++
	cp := *item
	cp.UID = fmt.Sprintf("ha-%d", m.nextUID)
	m.items[entityID] = append(m.items[entityID], cp)
	return cp.UID, nil
}

// findByUIDOrTitle mirrors HA's real todo service resolution
// (_find_by_uid_or_summary): match by UID first if given, else by title.
func findByUIDOrTitle(items []model.Item, uid, title string) int {
	if uid != "" {
		for i, h := range items {
			if h.UID == uid {
				return i
			}
		}
	}
	for i, h := range items {
		if h.Title == title {
			return i
		}
	}
	return -1
}

func (m *mockHA) UpdateItem(_ context.Context, entityID, haUID, currentTitle string, item *model.Item) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	items := m.items[entityID]
	i := findByUIDOrTitle(items, haUID, currentTitle)
	if i < 0 {
		return fmt.Errorf("item %q (uid=%q) not found in %s", currentTitle, haUID, entityID)
	}
	items[i].Title = item.Title
	items[i].Description = item.Description
	items[i].DueDate = item.DueDate
	items[i].Priority = item.Priority
	items[i].Completed = item.Completed
	items[i].ModifiedAt = item.ModifiedAt
	return nil
}

func (m *mockHA) RemoveItem(_ context.Context, entityID, haUID, title string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	items := m.items[entityID]
	i := findByUIDOrTitle(items, haUID, title)
	if i < 0 {
		return fmt.Errorf("item %q (uid=%q) not found in %s", title, haUID, entityID)
	}
	m.items[entityID] = append(items[:i], items[i+1:]...)
	return nil
}

func (m *mockHA) getItems(entityID string) []model.Item {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.items[entityID]
}

// --- Mock Calendar Source ------------------------------------------------

// mockHACalendar embeds *mockHA (satisfying HASource) and adds CalendarSource
// methods, so [NewReconciler]'s type assertion picks it up for calendar
// mirroring tests. mockHA alone deliberately does NOT implement
// CalendarSource, so non-calendar tests exercise the "mirroring disabled"
// path the same way a plain HASource-only adapter would.
type mockHACalendar struct {
	*mockHA

	mu       sync.Mutex
	events   map[string]map[string]*model.Item // entityID -> uid -> mirrored item
	nextUID  int
	mutable  bool // CalendarSupportsMutation return value
	failNext bool // forces the next Create/Update call to fail, once
	listFail bool // forces ListCalendarEvents to fail
}

func newMockHACalendar() *mockHACalendar {
	return &mockHACalendar{
		mockHA:  newMockHA(),
		events:  make(map[string]map[string]*model.Item),
		mutable: true,
	}
}

func (m *mockHACalendar) CalendarSupportsMutation(_ context.Context, _ string) (bool, error) {
	return m.mutable, nil
}

func (m *mockHACalendar) ListCalendarEvents(_ context.Context, entityID string) ([]model.CalendarEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.listFail {
		return nil, fmt.Errorf("simulated list failure")
	}

	events := make([]model.CalendarEvent, 0, len(m.events[entityID]))
	for uid, item := range m.events[entityID] {
		events = append(events, model.CalendarEvent{UID: uid, DueDate: item.DueDate})
	}
	return events, nil
}

func (m *mockHACalendar) CreateCalendarEvent(_ context.Context, entityID string, item *model.Item) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.failNext {
		m.failNext = false
		return "", fmt.Errorf("simulated create failure")
	}

	m.nextUID++
	uid := fmt.Sprintf("cal-%d", m.nextUID)
	cp := *item
	if m.events[entityID] == nil {
		m.events[entityID] = make(map[string]*model.Item)
	}
	m.events[entityID][uid] = &cp
	return uid, nil
}

func (m *mockHACalendar) UpdateCalendarEvent(_ context.Context, entityID, uid string, item *model.Item) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.failNext {
		m.failNext = false
		return fmt.Errorf("simulated update failure")
	}

	events := m.events[entityID]
	if events == nil || events[uid] == nil {
		return fmt.Errorf("calendar event %q not found on %s", uid, entityID)
	}
	cp := *item
	events[uid] = &cp
	return nil
}

func (m *mockHACalendar) DeleteCalendarEvent(_ context.Context, entityID, uid string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	events := m.events[entityID]
	if events == nil || events[uid] == nil {
		return fmt.Errorf("calendar event %q not found on %s", uid, entityID)
	}
	delete(events, uid)
	return nil
}

func (m *mockHACalendar) getEvent(entityID, uid string) *model.Item {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.events[entityID] == nil {
		return nil
	}
	return m.events[entityID][uid]
}

func (m *mockHACalendar) eventCount(entityID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.events[entityID])
}

// --- Mock State Store --------------------------------------------------------

type mockStore struct {
	mu     sync.Mutex
	items  map[int64]*state.Item
	nextID int64
}

func newMockStore() *mockStore {
	return &mockStore{items: make(map[int64]*state.Item)}
}

func (m *mockStore) seed(items ...*state.Item) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, item := range items {
		m.nextID++
		item.ID = m.nextID
		m.items[item.ID] = item
	}
}

func (m *mockStore) GetItemByRemindersUID(_ context.Context, uid string) (*state.Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, item := range m.items {
		if item.RemindersUID == uid {
			cp := *item
			return &cp, nil
		}
	}
	return nil, nil
}

func (m *mockStore) GetItemByHAUID(_ context.Context, uid string) (*state.Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, item := range m.items {
		if item.HAUID == uid {
			cp := *item
			return &cp, nil
		}
	}
	return nil, nil
}

func (m *mockStore) GetAllItemsForList(_ context.Context, listName string) ([]*state.Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*state.Item
	for _, item := range m.items {
		if item.ListName == listName {
			cp := *item
			result = append(result, &cp)
		}
	}
	return result, nil
}

func (m *mockStore) UpsertItem(_ context.Context, item *state.Item) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if item.ID == 0 {
		// Check for existing by RemindersUID.
		for _, existing := range m.items {
			if item.RemindersUID != "" && existing.RemindersUID == item.RemindersUID {
				item.ID = existing.ID
				*existing = *item
				return nil
			}
		}
		m.nextID++
		item.ID = m.nextID
	}
	cp := *item
	m.items[item.ID] = &cp
	return nil
}

func (m *mockStore) DeleteItem(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.items, id)
	return nil
}

func (m *mockStore) IsEmpty(_ context.Context) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.items) == 0, nil
}

func (m *mockStore) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.items)
}
