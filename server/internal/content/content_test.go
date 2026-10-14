package content

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"opentalk/server/internal/platform/httpapi"
	"opentalk/server/internal/platform/testdb"
)

// The board list is exercised against a real MySQL schema because the cursor and ordering behaviour
// are SQL semantics, not formatting: an in-memory fake would not prove the tuple comparison works.
const testOrigin = "http://127.0.0.1:5173"

type boardItem struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Kind        string `json:"kind"`
	SortOrder   int    `json:"sort_order"`
	IsArchived  bool   `json:"is_archived"`
}

type boardPage struct {
	Items      []boardItem `json:"items"`
	HasMore    bool        `json:"has_more"`
	NextCursor string      `json:"next_cursor"`
}

type response struct {
	status int
	body   []byte
}

func TestBoardListing(t *testing.T) {
	dsn := testdb.DSNFromEnv()
	if dsn == "" {
		t.Skip("set OPENTALK_TEST_MYSQL_DSN to run content integration checks")
	}
	db, cleanup, err := testdb.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	store := Store{DB: db}
	mux := http.NewServeMux()
	Register(mux, Deps{Store: store, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil))})
	handler := httpapi.Middleware(testOrigin, slog.New(slog.NewJSONHandler(io.Discard, nil)), mux)

	// An installation with no boards must serialise the list as [] rather than null, so a client can
	// iterate without a nil check.
	empty := call(t, handler, "/api/v1/boards")
	expectStatus(t, empty, 200)
	if !strings.Contains(string(empty.body), `"items":[]`) {
		t.Fatalf("an empty list must serialise as []: %s", string(empty.body))
	}
	if page := decodePage(t, empty); page.HasMore || page.NextCursor != "" {
		t.Fatalf("empty list reported more pages: %+v", page)
	}

	// Seeding is idempotent: the second run must report the existing count and insert nothing.
	if existing, err := store.SeedBoards(context.Background()); err != nil || existing != 0 {
		t.Fatalf("first seed: existing=%d err=%v", existing, err)
	}
	if existing, err := store.SeedBoards(context.Background()); err != nil || existing != 2 {
		t.Fatalf("second seed should report 2 existing boards: existing=%d err=%v", existing, err)
	}
	var total int
	if err := db.QueryRow("SELECT COUNT(*) FROM boards").Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Fatalf("re-running the seed duplicated boards: count=%d", total)
	}

	// The seeded boards come back in (sort_order, id) order with the expected kinds.
	page := decodePage(t, call(t, handler, "/api/v1/boards"))
	if len(page.Items) != 2 || page.HasMore || page.NextCursor != "" {
		t.Fatalf("unexpected full page: %+v", page)
	}
	if page.Items[0].Kind != "discussion" || page.Items[1].Kind != "qa" {
		t.Fatalf("unexpected board kinds: %+v", page.Items)
	}
	if page.Items[0].SortOrder != 10 || page.Items[1].SortOrder != 20 {
		t.Fatalf("boards are not ordered by sort_order: %+v", page.Items)
	}
	for _, item := range page.Items {
		if item.ID == "" || item.Name == "" || item.Description == "" {
			t.Fatalf("board is missing a required field: %+v", item)
		}
		if _, err := strconv.ParseUint(item.ID, 10, 64); err != nil {
			t.Fatalf("board id is not a decimal string: %q", item.ID)
		}
	}

	// limit=1 must report a next page and the cursor must continue where the first page stopped.
	first := decodePage(t, call(t, handler, "/api/v1/boards?limit=1"))
	if len(first.Items) != 1 || !first.HasMore || first.NextCursor == "" {
		t.Fatalf("expected one item and a cursor: %+v", first)
	}
	second := decodePage(t, call(t, handler, "/api/v1/boards?limit=1&cursor="+first.NextCursor))
	if len(second.Items) != 1 || second.HasMore || second.NextCursor != "" {
		t.Fatalf("the last page must not advertise another: %+v", second)
	}
	if second.Items[0].ID == first.Items[0].ID {
		t.Fatal("the cursor returned the same row again")
	}

	// A cursor issued for a different list is rejected, which is what binding a cursor to its scope
	// means in practice.
	foreign := call(t, handler, "/api/v1/boards?cursor="+encodeCursor("topics", 10, 1))
	expectStatus(t, foreign, 400)
	expectCode(t, foreign, "CURSOR_INVALID")
	for _, bad := range []string{"not-base64!!", encodeCursor("boards", 10) + "x"} {
		expectStatus(t, call(t, handler, "/api/v1/boards?cursor="+bad), 400)
	}

	// limit is bounded by the contract; out-of-range and non-numeric values are refused rather than
	// silently clamped, so a client cannot believe it received a full page.
	for _, limit := range []string{"0", "101", "-1", "abc"} {
		res := call(t, handler, "/api/v1/boards?limit="+limit)
		expectStatus(t, res, 422)
		expectCode(t, res, "INVALID_REQUEST")
	}

	// Archived boards stay visible: the flag only stops new topics.
	if _, err := db.Exec("UPDATE boards SET is_archived=TRUE WHERE kind='discussion'"); err != nil {
		t.Fatal(err)
	}
	archived := decodePage(t, call(t, handler, "/api/v1/boards"))
	if len(archived.Items) != 2 || !archived.Items[0].IsArchived {
		t.Fatalf("an archived board disappeared from the list: %+v", archived.Items)
	}
}

func call(t *testing.T, handler http.Handler, path string) response {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest("GET", path, nil))
	return response{status: recorder.Code, body: recorder.Body.Bytes()}
}

func decodePage(t *testing.T, res response) boardPage {
	t.Helper()
	var page boardPage
	if err := json.Unmarshal(res.body, &page); err != nil {
		t.Fatalf("cannot decode %s: %v", string(res.body), err)
	}
	return page
}

func expectStatus(t *testing.T, res response, want int) {
	t.Helper()
	if res.status != want {
		t.Fatalf("status=%d want=%d body=%s", res.status, want, string(res.body))
	}
}

func expectCode(t *testing.T, res response, want string) {
	t.Helper()
	var body struct {
		Code      string `json:"code"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(res.body, &body); err != nil {
		t.Fatalf("cannot decode %s: %v", string(res.body), err)
	}
	if body.Code != want {
		t.Fatalf("code=%q want=%q body=%s", body.Code, want, string(res.body))
	}
	if body.RequestID == "" {
		t.Fatalf("error response has no request_id: %s", string(res.body))
	}
}
