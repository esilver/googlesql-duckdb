package internal

import (
	"errors"
	"testing"
)

type fakeLastInsertResult struct {
	id  int64
	err error
}

func (r fakeLastInsertResult) LastInsertId() (int64, error) {
	return r.id, r.err
}

func (r fakeLastInsertResult) RowsAffected() (int64, error) {
	return 0, nil
}

func TestLastInsertIdOnlyMasksUnsupportedDuckDBError(t *testing.T) {
	t.Run("driver result unsupported", func(t *testing.T) {
		res := &Result{result: fakeLastInsertResult{err: errors.New("LastInsertId is not supported")}}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatalf("LastInsertId error = %v; want nil", err)
		}
		if id != 0 {
			t.Fatalf("LastInsertId = %d; want 0", id)
		}
	})

	t.Run("exec result unsupported", func(t *testing.T) {
		res := &execResult{result: fakeLastInsertResult{err: errors.New("duckdb: LastInsertId is not supported")}}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatalf("LastInsertId error = %v; want nil", err)
		}
		if id != 0 {
			t.Fatalf("LastInsertId = %d; want 0", id)
		}
	})

	t.Run("unexpected error propagates", func(t *testing.T) {
		wantErr := errors.New("driver connection is closed")
		res := &Result{result: fakeLastInsertResult{err: wantErr}}
		id, err := res.LastInsertId()
		if !errors.Is(err, wantErr) {
			t.Fatalf("LastInsertId error = %v; want %v", err, wantErr)
		}
		if id != 0 {
			t.Fatalf("LastInsertId = %d; want 0", id)
		}
	})

	t.Run("success passes through", func(t *testing.T) {
		res := &Result{result: fakeLastInsertResult{id: 42}}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatalf("LastInsertId error = %v; want nil", err)
		}
		if id != 42 {
			t.Fatalf("LastInsertId = %d; want 42", id)
		}
	})
}
