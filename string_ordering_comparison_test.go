package googlesqlite_test

// Regression coverage for STRING ordering comparisons (<, <=, >, >=) over
// runtime values. A runtime STRING (a stored column, a UDF result like
// NEW_UUID(), a bound parameter) reaches DuckDB as the value-layer envelope
// (base64 of {"header":"string","body":...}). Base64 text is NOT monotonic
// with the payload bytes — the base64 alphabet's value order (A-Z < a-z <
// 0-9) disagrees with ASCII order (0-9 < A-Z < a-z) — so comparing
// envelopes as VARCHAR inverts the order whenever the first differing
// 6-bit group crosses character classes. Caught (flakily, ~2.5% of UUIDs)
// by the uuid spec: NEW_UUID() >= "00000000-..." returned false for UUIDs
// whose body starts '3' followed by a lowercase hex digit. The formatter
// now unwraps both operands of an ordering comparison with
// googlesqlite_plain_string, so the payloads compare in UTF-8 byte order
// (== code-point order, GoogleSQL's STRING ordering). Equality stays on
// the envelope form (deterministic, injective encoding).
//
// The pairs below are DETERMINISTIC instances of the inversion class:
// base64(`{"header":"string","body":"3a` ...) starts eyJ...Ijoi M2E
// ('M','2'), while the "09" envelope continues 'M','D' — ASCII '2' < 'D',
// so the envelope order said "3a" < "09" even though "3a" > "09".

import "testing"

func TestStringOrderingComparison_EnvelopeOrderInversion(t *testing.T) {
	t.Parallel()
	db := openSeamDB(t, "string_ordering")
	seamExec(t, db,
		"CREATE TABLE s (id INT64, v STRING, w STRING)",
		// id=1: the uuid-spec instance — a UUID from the inverting class
		// vs the all-zero UUID.
		`INSERT INTO s (id, v, w) VALUES (1, '3a1d0fbd-3ea9-4687-9b19-034af1ad4003', '00000000-0000-0000-0000-000000000000')`,
		// id=2: the minimal inverting pair.
		`INSERT INTO s (id, v, w) VALUES (2, '3a', '09')`,
		// id=3: NULL operand must stay NULL through the unwrap.
		`INSERT INTO s (id, v, w) VALUES (3, NULL, 'x')`,
		// id=4: letter-case pair for code-point order.
		`INSERT INTO s (id, v, w) VALUES (4, 'apple', 'Zebra')`,
	)

	// Stored column vs literal: every ordering operator, both directions.
	wantBool(t, db, `SELECT v >= '00000000-0000-0000-0000-000000000000' FROM s WHERE id = 1`, true)
	wantBool(t, db, `SELECT v > w FROM s WHERE id = 1`, true)
	wantBool(t, db, `SELECT v < w FROM s WHERE id = 1`, false)
	wantBool(t, db, `SELECT v <= w FROM s WHERE id = 1`, false)
	wantBool(t, db, `SELECT w < v FROM s WHERE id = 1`, true)

	// Column vs column (both operands runtime envelopes).
	wantBool(t, db, `SELECT v > w FROM s WHERE id = 2`, true)
	wantBool(t, db, `SELECT v >= w FROM s WHERE id = 2`, true)
	wantBool(t, db, `SELECT w < v FROM s WHERE id = 2`, true)

	// Code-point (binary UTF-8) order across letter case: 'Zebra' < 'apple'
	// (uppercase sorts below lowercase), with both operands stored.
	wantBool(t, db, `SELECT w < v FROM s WHERE id = 4`, true)
	wantBool(t, db, `SELECT v > 'Zebra' FROM s WHERE id = 4`, true)

	// Equality is untouched by the unwrap and still correct.
	wantBool(t, db, `SELECT v = '3a' FROM s WHERE id = 2`, true)
	wantBool(t, db, `SELECT v <> w FROM s WHERE id = 2`, true)

	// NULL propagates: an ordering comparison with a NULL side is NULL.
	wantBool(t, db, `SELECT (v > w) IS NULL FROM s WHERE id = 3`, true)

	// The volatile-UDF shape from the uuid spec, made deterministic by
	// LOWER() (a value-layer UDF result, i.e. a runtime envelope operand).
	wantBool(t, db, `SELECT LOWER('3A1D0FBD') >= '00000000' FROM s WHERE id = 1`, true)

	// The uuid spec's exact shape: UUID-typed operands (the analyzer
	// types NEW_UUID() and the coerced literal as UUID; the wire form is
	// the canonical-text STRING envelope). Any UUID is >= the zero UUID,
	// so this is deterministic — pre-fix it flipped false for the ~2.5%
	// of UUIDs in the base64-inverting class.
	for i := 0; i < 8; i++ {
		wantBool(t, db, `SELECT NEW_UUID() >= '00000000-0000-0000-0000-000000000000'`, true)
		wantBool(t, db, `SELECT NEW_UUID() <= 'ffffffff-ffff-ffff-ffff-ffffffffffff'`, true)
	}
}
