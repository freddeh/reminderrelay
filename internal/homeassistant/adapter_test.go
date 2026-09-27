package homeassistant

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"

	haclient "github.com/mkelcik/go-ha-client/v2"

	"github.com/njoerd114/reminderrelay/internal/model"
)

// fakeRESTClient simulates HA's todo REST services in-memory, so Adapter's
// UID-diffing and retry logic can be exercised without a real HA instance.
type fakeRESTClient struct {
	mu    sync.Mutex
	items map[string][]haTodoItem // entityID -> items
	seq   int
}

func newFakeRESTClient() *fakeRESTClient {
	return &fakeRESTClient{items: make(map[string][]haTodoItem)}
}

func (f *fakeRESTClient) Ping(_ context.Context) error { return nil }

func (f *fakeRESTClient) CallService(_ context.Context, _, service string, body io.Reader) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	var data map[string]any
	if err := json.NewDecoder(body).Decode(&data); err != nil {
		return fmt.Errorf("decode request: %w", err)
	}
	entityID, _ := data["entity_id"].(string)

	switch service {
	case serviceAddItem:
		title, _ := data["item"].(string)
		desc, _ := data["description"].(string)

		f.seq++
		f.items[entityID] = append(f.items[entityID], haTodoItem{
			UID:         fmt.Sprintf("ha-%d", f.seq),
			Summary:     title,
			Description: desc,
			Status:      statusNeedsAction,
		})
		return nil
	default:
		return fmt.Errorf("fakeRESTClient: unhandled service %s", service)
	}
}

func (f *fakeRESTClient) CallServiceWithResponse(_ context.Context, _, service string, body io.Reader) (haclient.ServiceCallResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if service != serviceGetItems {
		return haclient.ServiceCallResponse{}, fmt.Errorf("fakeRESTClient: unhandled service %s", service)
	}

	var data map[string]any
	if err := json.NewDecoder(body).Decode(&data); err != nil {
		return haclient.ServiceCallResponse{}, fmt.Errorf("decode request: %w", err)
	}
	entityID, _ := data["entity_id"].(string)

	raw, err := json.Marshal(haItemsResponse{Items: f.items[entityID]})
	if err != nil {
		return haclient.ServiceCallResponse{}, err
	}
	return haclient.ServiceCallResponse{
		ServiceResponse: map[string]json.RawMessage{entityID: raw},
	}, nil
}

func (f *fakeRESTClient) seedItems(entityID string, items ...haTodoItem) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[entityID] = append(f.items[entityID], items...)
}

// ---------------------------------------------------------------------------
// AddItem: UID returned via diff
// ---------------------------------------------------------------------------

func TestAdapter_AddItem_ReturnsNewUID(t *testing.T) {
	rest := newFakeRESTClient()
	rest.seedItems("todo.shopping", haTodoItem{UID: "ha-existing", Summary: "Existing"})

	a := NewAdapterWithClient(rest, slog.Default())

	uid, err := a.AddItem(context.Background(), "todo.shopping", &model.Item{Title: "Buy milk"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if uid == "" || uid == "ha-existing" {
		t.Fatalf("uid = %q, want a freshly generated UID distinct from pre-existing items", uid)
	}

	items, err := a.GetItems(context.Background(), "todo.shopping")
	if err != nil {
		t.Fatalf("GetItems: %v", err)
	}
	var found bool
	for _, it := range items {
		if it.UID == uid {
			found = true
			if it.Title != "Buy milk" {
				t.Errorf("item at returned UID has title %q, want %q", it.Title, "Buy milk")
			}
		}
	}
	if !found {
		t.Errorf("returned UID %q does not match any item in HA", uid)
	}
}

func TestAdapter_AddItem_NoNewItemAppeared_ReturnsError(t *testing.T) {
	// A RESTClient whose add_item call "succeeds" but never actually creates
	// anything (e.g. HA silently rejected it) should surface as an error
	// rather than a bogus empty UID.
	rest := &noopAddRESTClient{fakeRESTClient: newFakeRESTClient()}

	a := NewAdapterWithClient(rest, slog.Default())
	_, err := a.AddItem(context.Background(), "todo.shopping", &model.Item{Title: "Buy milk"})
	if err == nil {
		t.Fatal("expected error when no new item appears after add_item, got nil")
	}
}

// noopAddRESTClient accepts add_item calls without ever mutating state.
type noopAddRESTClient struct {
	*fakeRESTClient
}

func (n *noopAddRESTClient) CallService(_ context.Context, _, service string, _ io.Reader) error {
	if service == serviceAddItem {
		return nil // pretend success, but don't touch n.items
	}
	return fmt.Errorf("unhandled service %s", service)
}

// ---------------------------------------------------------------------------
// diffNewItemUID unit tests
// ---------------------------------------------------------------------------

func TestDiffNewItemUID_SingleCandidate(t *testing.T) {
	before := []model.Item{{UID: "ha-1", Title: "Existing"}}
	after := []model.Item{
		{UID: "ha-1", Title: "Existing"},
		{UID: "ha-2", Title: "Buy milk"},
	}

	uid, ok := diffNewItemUID(before, after, "Buy milk")
	if !ok || uid != "ha-2" {
		t.Errorf("diffNewItemUID = (%q, %v), want (%q, true)", uid, ok, "ha-2")
	}
}

func TestDiffNewItemUID_MultipleCandidates_PrefersTitleMatch(t *testing.T) {
	before := []model.Item{{UID: "ha-1", Title: "Existing"}}
	after := []model.Item{
		{UID: "ha-1", Title: "Existing"},
		{UID: "ha-2", Title: "Unrelated concurrent item"},
		{UID: "ha-3", Title: "Buy milk"},
	}

	uid, ok := diffNewItemUID(before, after, "Buy milk")
	if !ok || uid != "ha-3" {
		t.Errorf("diffNewItemUID = (%q, %v), want (%q, true)", uid, ok, "ha-3")
	}
}

func TestDiffNewItemUID_NoCandidates(t *testing.T) {
	before := []model.Item{{UID: "ha-1", Title: "Existing"}}
	after := []model.Item{{UID: "ha-1", Title: "Existing"}}

	_, ok := diffNewItemUID(before, after, "Buy milk")
	if ok {
		t.Error("diffNewItemUID = ok, want false when nothing new appeared")
	}
}
