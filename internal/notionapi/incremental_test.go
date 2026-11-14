package notionapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openclaw/notcrawl/internal/store"
)

// incrementalFixture is a fake Notion API with mutable page timestamps and
// block text, plus request counters for the content endpoints incremental
// sync must avoid when a page is unchanged.
type incrementalFixture struct {
	edited          map[string]string
	blockText       map[string]string
	failChildren    map[string]bool
	blockRequests   int
	commentRequests int
}

func newIncrementalFixture(ids ...string) *incrementalFixture {
	f := &incrementalFixture{
		edited:       map[string]string{},
		blockText:    map[string]string{},
		failChildren: map[string]bool{},
	}
	for _, id := range ids {
		f.edited[id] = "2026-01-01T00:00:00Z"
		f.blockText[id] = id + "body"
	}
	return f
}

func (f *incrementalFixture) pageIDs() []string {
	ids := make([]string, 0, len(f.edited))
	for id := range f.edited {
		ids = append(ids, id)
	}
	return ids
}

func (f *incrementalFixture) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/users":
		_ = json.NewEncoder(w).Encode(obj{"results": []any{}, "has_more": false})
	case r.URL.Path == "/search":
		var body obj
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		results := []any{}
		if body.mapObj("filter").string("value") == "page" {
			for _, id := range f.pageIDs() {
				results = append(results, obj{
					"object":           "page",
					"id":               id,
					"archived":         false,
					"in_trash":         false,
					"last_edited_time": f.edited[id],
					"parent":           obj{"type": "workspace", "workspace": true},
					"properties": obj{"title": obj{"type": "title", "title": []any{
						obj{"plain_text": id + "title"},
					}}},
				})
			}
		}
		_ = json.NewEncoder(w).Encode(obj{"results": results, "has_more": false})
	case strings.HasPrefix(r.URL.Path, "/blocks/"):
		id := strings.Split(r.URL.Path, "/")[2]
		f.blockRequests++
		if f.failChildren[id] {
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(obj{"object": "error", "status": 500, "code": "internal_server_error", "message": "synthetic children failure"})
			return
		}
		_ = json.NewEncoder(w).Encode(obj{
			"results": []any{obj{
				"object": "block", "id": id + "block", "type": "paragraph", "has_children": false,
				"paragraph": obj{"rich_text": []any{obj{"plain_text": f.blockText[id]}}},
			}},
			"has_more": false,
		})
	case r.URL.Path == "/comments":
		f.commentRequests++
		id := r.URL.Query().Get("block_id")
		_ = json.NewEncoder(w).Encode(obj{
			"results": []any{obj{
				"id":         id + "comment",
				"rich_text":  []any{obj{"plain_text": id + "commentword"}},
				"created_by": obj{"id": "user1"},
			}},
			"has_more": false,
		})
	default:
		http.Error(w, "unexpected request", http.StatusBadRequest)
	}
}

func newIncrementalTest(t *testing.T, f *incrementalFixture) (Client, *store.Store) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	st := openTestStore(t)
	t.Cleanup(func() { _ = st.Close() })
	return Client{BaseURL: server.URL, Version: "2026-03-11", Token: "secret", HTTP: server.Client()}, st
}

func TestSyncSkipsUnchangedPagesOnSecondRun(t *testing.T) {
	fixture := newIncrementalFixture("page1")
	client, st := newIncrementalTest(t, fixture)
	ctx := context.Background()

	first, err := client.Sync(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if first.Pages != 1 || first.Skipped != 0 || first.Failed != 0 || first.Blocks != 1 || first.Comments != 1 {
		t.Fatalf("first sync = %+v", first)
	}
	fixture.blockRequests, fixture.commentRequests = 0, 0

	second, err := client.Sync(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if second.Pages != 1 || second.Skipped != 1 || second.Failed != 0 || second.Blocks != 0 || second.Comments != 0 {
		t.Fatalf("second sync = %+v", second)
	}
	if fixture.blockRequests != 0 || fixture.commentRequests != 0 {
		t.Fatalf("unchanged page refetched: blocks=%d comments=%d", fixture.blockRequests, fixture.commentRequests)
	}
	blocks, err := st.PageBlocks(ctx, "page1")
	if err != nil || len(blocks) != 1 || blocks[0].Text != "page1body" {
		t.Fatalf("skipped page lost its blocks: %+v, %v", blocks, err)
	}
	comments, err := st.PageComments(ctx, "page1")
	if err != nil || len(comments) != 1 {
		t.Fatalf("skipped page lost its comments: %+v, %v", comments, err)
	}
	results, err := st.Search(ctx, "page1body", 10)
	if err != nil || len(results) != 1 {
		t.Fatalf("skipped page lost from search: %v, %v", results, err)
	}
}

func TestSyncRefetchesPageWhenLastEditedTimeChanges(t *testing.T) {
	fixture := newIncrementalFixture("page1")
	client, st := newIncrementalTest(t, fixture)
	ctx := context.Background()
	if _, err := client.Sync(ctx, st); err != nil {
		t.Fatal(err)
	}

	fixture.edited["page1"] = "2026-02-01T00:00:00Z"
	fixture.blockText["page1"] = "rewritten"
	fixture.blockRequests, fixture.commentRequests = 0, 0

	summary, err := client.Sync(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Pages != 1 || summary.Skipped != 0 || summary.Failed != 0 || summary.Blocks != 1 {
		t.Fatalf("edited page sync = %+v", summary)
	}
	if fixture.blockRequests == 0 || fixture.commentRequests == 0 {
		t.Fatalf("edited page was not refetched: blocks=%d comments=%d", fixture.blockRequests, fixture.commentRequests)
	}
	blocks, err := st.PageBlocks(ctx, "page1")
	if err != nil || len(blocks) != 1 || blocks[0].Text != "rewritten" {
		t.Fatalf("edited page blocks = %+v, %v", blocks, err)
	}
}

func TestSyncIsolatesPageFailureAndKeepsOmittedPages(t *testing.T) {
	fixture := newIncrementalFixture("good", "broken")
	client, st := newIncrementalTest(t, fixture)
	ctx := context.Background()
	if err := st.UpsertPage(ctx, store.Page{ID: "omitted", Title: "omittedtitle", Alive: true, Source: SourceName, SyncedAt: 1}); err != nil {
		t.Fatal(err)
	}
	fixture.failChildren["broken"] = true

	summary, err := client.Sync(ctx, st)
	if err != nil {
		t.Fatalf("page failure must not abort the sync: %v", err)
	}
	if summary.Pages != 2 || summary.Failed != 1 || summary.Skipped != 0 {
		t.Fatalf("isolated failure sync = %+v", summary)
	}
	blocks, err := st.PageBlocks(ctx, "good")
	if err != nil || len(blocks) != 1 || blocks[0].Text != "goodbody" {
		t.Fatalf("healthy page blocks = %+v, %v", blocks, err)
	}
	if synced, err := st.HasSyncState(ctx, SourceName, "page_blocks", "broken"); err != nil || synced {
		t.Fatalf("failed page marked complete: synced=%v, %v", synced, err)
	}
	if synced, err := st.HasSyncState(ctx, SourceName, "workspace", "default"); err != nil || synced {
		t.Fatalf("failed run marked workspace complete: synced=%v, %v", synced, err)
	}
	// The failure disables omission reconciliation: the undiscovered page
	// must survive this run.
	if live, err := st.RecordHasLiveSource(ctx, "page", "omitted", SourceName); err != nil || !live {
		t.Fatalf("omitted page retired by a failed run: live=%v, %v", live, err)
	}

	// Once the children endpoint recovers, the next run re-fetches the
	// failed page and marks it complete.
	delete(fixture.failChildren, "broken")
	summary, err = client.Sync(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Failed != 0 || summary.Skipped != 1 {
		t.Fatalf("recovery sync = %+v", summary)
	}
	if synced, err := st.HasSyncState(ctx, SourceName, "page_blocks", "broken"); err != nil || !synced {
		t.Fatalf("recovered page not marked complete: synced=%v, %v", synced, err)
	}
}

func TestSyncRefetchesPageWithoutCompleteMarker(t *testing.T) {
	fixture := newIncrementalFixture("page1")
	client, st := newIncrementalTest(t, fixture)
	ctx := context.Background()
	if _, err := client.Sync(ctx, st); err != nil {
		t.Fatal(err)
	}
	// Simulate an interrupted run: the stored revision is current but the
	// previous block walk never finished.
	if err := st.ClearSyncState(ctx, SourceName, "page_blocks", "page1"); err != nil {
		t.Fatal(err)
	}
	fixture.blockRequests, fixture.commentRequests = 0, 0

	summary, err := client.Sync(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Pages != 1 || summary.Skipped != 0 || summary.Failed != 0 || summary.Blocks != 1 {
		t.Fatalf("interrupted page sync = %+v", summary)
	}
	if fixture.blockRequests == 0 || fixture.commentRequests == 0 {
		t.Fatalf("page without completion marker was skipped: blocks=%d comments=%d", fixture.blockRequests, fixture.commentRequests)
	}
	if synced, err := st.HasSyncState(ctx, SourceName, "page_blocks", "page1"); err != nil || !synced {
		t.Fatalf("refetched page not marked complete: synced=%v, %v", synced, err)
	}
}
