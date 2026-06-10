package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestServer starts an httptest server over a fresh engine session
// backed by a throwaway database (the ":memory:" flag path), so each
// test is isolated.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	dsn, cleanup, err := resolveDSN(":memory:")
	if err != nil {
		t.Fatalf("resolveDSN: %v", err)
	}
	t.Cleanup(cleanup)
	srv, err := newServer(context.Background(), dsn)
	if err != nil {
		t.Fatalf("newServer: %v", err)
	}
	ts := httptest.NewServer(srv.handler())
	t.Cleanup(func() {
		ts.Close()
		_ = srv.close()
	})
	return ts
}

// execResponse mirrors the /exec wire format.
type execResponse struct {
	Output  string      `json:"output"`
	Results []execEntry `json:"results"`
}

type execEntry struct {
	Statement    string     `json:"statement"`
	IsQuery      bool       `json:"isQuery"`
	Columns      []string   `json:"columns"`
	Rows         [][]string `json:"rows"`
	RowsAffected int64      `json:"rowsAffected"`
	Error        string     `json:"error"`
	ElapsedMs    float64    `json:"elapsedMs"`
	Timestamp    string     `json:"timestamp"`
}

func postExec(t *testing.T, ts *httptest.Server, sql string) execResponse {
	t.Helper()
	resp, err := http.Post(ts.URL+"/exec", "application/json",
		strings.NewReader(`{"sql":`+mustJSON(t, sql)+`}`))
	if err != nil {
		t.Fatalf("POST /exec: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /exec: status %d", resp.StatusCode)
	}
	var out execResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode /exec response: %v", err)
	}
	return out
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestExecQuery(t *testing.T) {
	ts := newTestServer(t)
	out := postExec(t, ts, "SELECT 1 + 1 AS two")
	if len(out.Results) != 1 {
		t.Fatalf("results = %d, want 1", len(out.Results))
	}
	r := out.Results[0]
	if r.Error != "" {
		t.Fatalf("unexpected error: %q", r.Error)
	}
	if !r.IsQuery {
		t.Error("isQuery = false, want true")
	}
	if len(r.Columns) != 1 || r.Columns[0] != "two" {
		t.Errorf("columns = %v, want [two]", r.Columns)
	}
	if len(r.Rows) != 1 || len(r.Rows[0]) != 1 || r.Rows[0][0] != "2" {
		t.Errorf("rows = %v, want [[2]]", r.Rows)
	}
	if r.Timestamp == "" {
		t.Error("timestamp missing")
	}
}

// TestExecRoundTrip is the acceptance round-trip: CREATE TABLE +
// INSERT + SELECT in three calls, BigQuery (GoogleSQL) dialect.
func TestExecRoundTrip(t *testing.T) {
	ts := newTestServer(t)

	out := postExec(t, ts, "CREATE TABLE songs (id INT64, title STRING)")
	if len(out.Results) != 1 || out.Results[0].Error != "" {
		t.Fatalf("CREATE failed: %+v", out.Results)
	}
	if out.Results[0].IsQuery {
		t.Error("CREATE reported as query")
	}

	out = postExec(t, ts,
		"INSERT INTO songs (id, title) VALUES (1, 'paranoid android'), (2, 'karma police')")
	if out.Results[0].Error != "" {
		t.Fatalf("INSERT failed: %q", out.Results[0].Error)
	}
	if out.Results[0].RowsAffected != 2 {
		t.Errorf("rowsAffected = %d, want 2", out.Results[0].RowsAffected)
	}

	// A GoogleSQL-only construct, so the BigQuery dialect is exercised
	// (CONCAT + SAFE_DIVIDE are googlesqlite_* UDF territory).
	out = postExec(t, ts,
		"SELECT CONCAT(title, '!') AS loud, SAFE_DIVIDE(id, 0) AS d FROM songs ORDER BY id")
	r := out.Results[0]
	if r.Error != "" {
		t.Fatalf("SELECT failed: %q", r.Error)
	}
	if len(r.Rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(r.Rows))
	}
	if r.Rows[0][0] != "paranoid android!" {
		t.Errorf("rows[0][0] = %q, want %q", r.Rows[0][0], "paranoid android!")
	}
	if r.Rows[0][1] != "NULL" {
		t.Errorf("SAFE_DIVIDE(1, 0) rendered %q, want NULL", r.Rows[0][1])
	}
}

func TestExecMultiStatement(t *testing.T) {
	ts := newTestServer(t)
	out := postExec(t, ts, `
		CREATE TABLE t (n INT64);
		INSERT INTO t VALUES (1), (2), (3);
		SELECT SUM(n) AS total FROM t;
	`)
	if len(out.Results) != 3 {
		t.Fatalf("results = %d, want 3", len(out.Results))
	}
	for i, r := range out.Results {
		if r.Error != "" {
			t.Fatalf("statement %d failed: %q", i, r.Error)
		}
	}
	last := out.Results[2]
	if len(last.Rows) != 1 || last.Rows[0][0] != "6" {
		t.Errorf("SUM rows = %v, want [[6]]", last.Rows)
	}
}

// TestExecErrorShape: a SQL error is HTTP 200 with the per-statement
// error field set, and execution continues to later statements
// (matching the wasm engine's stopOnError=false behaviour).
func TestExecErrorShape(t *testing.T) {
	ts := newTestServer(t)
	out := postExec(t, ts, "SELECT FROM nowhere; SELECT 42 AS ok;")
	if len(out.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(out.Results))
	}
	if out.Results[0].Error == "" {
		t.Error("first statement: error missing")
	}
	if out.Results[0].Columns == nil || out.Results[0].Rows == nil {
		t.Error("error entry must still carry columns/rows arrays (not null)")
	}
	if out.Results[1].Error != "" || out.Results[1].Rows[0][0] != "42" {
		t.Errorf("second statement = %+v, want 42 with no error", out.Results[1])
	}
}

func TestExecBadRequest(t *testing.T) {
	ts := newTestServer(t)

	resp, err := http.Post(ts.URL+"/exec", "application/json", strings.NewReader("{not json"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad JSON: status %d, want 400", resp.StatusCode)
	}
	var e struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil || e.Error == "" {
		t.Errorf("bad JSON: error shape missing (%v, %+v)", err, e)
	}

	get, err := http.Get(ts.URL + "/exec")
	if err != nil {
		t.Fatal(err)
	}
	get.Body.Close()
	if get.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /exec: status %d, want 405", get.StatusCode)
	}
}

func TestHealthz(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var h struct {
		OK bool `json:"ok"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil || !h.OK {
		t.Fatalf("healthz body wrong: %v %+v", err, h)
	}
}

func TestTablesAndSchema(t *testing.T) {
	ts := newTestServer(t)
	postExec(t, ts, "CREATE TABLE albums (id INT64, name STRING, year INT64)")

	resp, err := http.Get(ts.URL + "/tables")
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	if err := json.NewDecoder(resp.Body).Decode(&tables); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(tables) != 1 || tables[0] != "albums" {
		t.Fatalf("tables = %v, want [albums]", tables)
	}

	resp, err = http.Get(ts.URL + "/schema")
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Tables []struct {
			Name    string   `json:"name"`
			Columns []string `json:"columns"`
		} `json:"tables"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&schema); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(schema.Tables) != 1 || schema.Tables[0].Name != "albums" {
		t.Fatalf("schema = %+v", schema)
	}
	want := []string{"id", "name", "year"}
	got := schema.Tables[0].Columns
	if len(got) != len(want) {
		t.Fatalf("columns = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("columns[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestHistoryLifecycle(t *testing.T) {
	ts := newTestServer(t)
	postExec(t, ts, "SELECT 'remember me' AS m")

	resp, err := http.Get(ts.URL + "/history")
	if err != nil {
		t.Fatal(err)
	}
	var entries []execEntry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(entries) != 1 || entries[0].Statement != "SELECT 'remember me' AS m" {
		t.Fatalf("history = %+v", entries)
	}

	// Export as JSON, clear, then load it back.
	resp, err = http.Get(ts.URL + "/history/export?format=json")
	if err != nil {
		t.Fatal(err)
	}
	exportedBytes, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	exported := string(exportedBytes)
	if !json.Valid(exportedBytes) {
		t.Fatalf("export is not valid JSON: %q", exported)
	}

	if resp, err = http.Post(ts.URL+"/history/clear", "application/json", nil); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	resp, err = http.Get(ts.URL + "/history")
	if err != nil {
		t.Fatal(err)
	}
	entries = nil
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(entries) != 0 {
		t.Fatalf("history after clear = %+v, want empty", entries)
	}

	if resp, err = http.Post(ts.URL+"/history/load", "application/json",
		strings.NewReader(exported)); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	resp, err = http.Get(ts.URL + "/history")
	if err != nil {
		t.Fatal(err)
	}
	entries = nil
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(entries) != 1 {
		t.Fatalf("history after load = %d entries, want 1", len(entries))
	}
}

func TestCORSPreflight(t *testing.T) {
	ts := newTestServer(t)
	req, err := http.NewRequest(http.MethodOptions, ts.URL+"/exec", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "http://localhost:5173")
	req.Header.Set("Access-Control-Request-Method", "POST")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("preflight status = %d, want 204", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Errorf("Allow-Origin = %q, want http://localhost:5173", got)
	}

	// A non-local origin gets no CORS grant.
	req, _ = http.NewRequest(http.MethodOptions, ts.URL+"/exec", nil)
	req.Header.Set("Origin", "https://evil.example")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Allow-Origin for foreign origin = %q, want empty", got)
	}
}
