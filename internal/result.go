package internal

import "database/sql/driver"

type Result struct {
	conn   *Conn
	result driver.Result
}

func (r *Result) ChangedCatalog() *ChangedCatalog {
	return r.conn.cc
}

func (r *Result) LastInsertId() (int64, error) {
	if r.result == nil {
		return 0, nil
	}
	id, err := r.result.LastInsertId()
	if err != nil {
		// The DuckDB driver does not support LastInsertId (there is no
		// rowid concept). BigQuery has no last-insert-id either, and the
		// legacy SQLite backend returned an implementation-defined value.
		// Degrade to 0 instead of erroring so database/sql callers that
		// probe the Result keep working.
		return 0, nil
	}
	return id, nil
}

func (r *Result) RowsAffected() (int64, error) {
	if r.result == nil {
		return 0, nil
	}
	return r.result.RowsAffected()
}

// execResult wraps the inner driver/sql result returned by a prepared
// statement's Exec so LastInsertId degrades to 0 instead of surfacing
// the DuckDB driver's "LastInsertId is not supported" error (same
// contract as Result.LastInsertId above).
type execResult struct {
	result interface {
		LastInsertId() (int64, error)
		RowsAffected() (int64, error)
	}
}

func (r *execResult) LastInsertId() (int64, error) {
	id, err := r.result.LastInsertId()
	if err != nil {
		return 0, nil
	}
	return id, nil
}

func (r *execResult) RowsAffected() (int64, error) {
	return r.result.RowsAffected()
}
