# String literal coercion to DATE / TIMESTAMP

## Status

Accepted. Matches the GoogleSQL specification.

## Context

While reviewing analyzer behavior on operator overloads, three queries were
flagged as potentially erroneous:

```sql
SELECT '2022-09-09' - INTERVAL 1 DAY;
SELECT '2022-09-09' > TIMESTAMP '2022-09-09 00:00:00+00';
SELECT CAST('2022-09-09' AS DATE) - INTERVAL 1 DAY;
```

`googlesqlite` (via `go-googlesql` v0.2.1) and its predecessor, the upstream
wasm bridge, both accept all three with identical results:

| Query                                                            | Result                  |
|------------------------------------------------------------------|-------------------------|
| `'2022-09-09' - INTERVAL 1 DAY`                                  | `2022-09-08T00:00:00`   |
| `'2022-09-09' > TIMESTAMP '2022-09-09 00:00:00+00'`              | `true`                  |
| `CAST('2022-09-09' AS DATE) - INTERVAL 1 DAY`                    | `2022-09-08T00:00:00`   |

There is no implementation divergence. The open question was whether the
analyzer ought to reject the first two as type errors.

## Decision

The behavior is correct per the GoogleSQL spec. From
`docs/third_party/googlesql-docs/conversion_rules.md` (`Literal coercion`):

> Literal coercion is needed when the actual literal type is different from the
> type expected by the function in question. For example, if function `func()`
> takes a `DATE` argument, then the expression `func("2014-09-27")` is valid
> because the string literal `"2014-09-27"` is coerced to `DATE`.

The arithmetic and comparison operators here expect `DATE`/`TIMESTAMP` operands,
so the string literals coerce per the documented rule. No upstream issue is
filed against `go-googlesql`, and no strict-mode toggle is added to
`googlesqlite`.

## Notes

- The first and third queries render `2022-09-08T00:00:00` rather than a bare
  `2022-09-08`. This is the internal `DATE` rendering used by both
  implementations, consistent across the predecessor and the current driver, not
  a regression from the `go-googlesql` v0.1.0 to v0.2.1 bump.
- Coercion applies only to **literals**. Per the same section, `STRING` query
  parameters and column references are not silently coerced to
  `DATE`/`TIMESTAMP` for arithmetic, so an explicit `CAST` is required.
