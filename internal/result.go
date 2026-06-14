package internal

import (
	"database/sql/driver"
	"strings"
)

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
	return normalizeLastInsertID(id, err)
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
	return normalizeLastInsertID(id, err)
}

func (r *execResult) RowsAffected() (int64, error) {
	return r.result.RowsAffected()
}

func normalizeLastInsertID(id int64, err error) (int64, error) {
	if err == nil {
		return id, nil
	}
	// The DuckDB driver does not support LastInsertId (there is no rowid
	// concept), and BigQuery has no last-insert-id either, so degrade only
	// that unsupported surface to 0. Other driver errors should remain
	// visible.
	if isUnsupportedLastInsertIDError(err) {
		return 0, nil
	}
	return 0, err
}

func isUnsupportedLastInsertIDError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "lastinsertid is not supported") ||
		strings.Contains(msg, "last insert id is not supported")
}
