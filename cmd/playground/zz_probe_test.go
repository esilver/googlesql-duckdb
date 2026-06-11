package main

import (
	"io"
	"net/http"
	"testing"
)

func TestZZProbeTables(t *testing.T) {
	ts := newTestServer(t)
	postExec(t, ts, "CREATE TABLE albums (id INT64, name STRING, year INT64)")
	resp, err := http.Get(ts.URL + "/tables")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	t.Fatalf("status=%d body=%s", resp.StatusCode, b)
}
