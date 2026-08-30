# google-sheets-sql

A Go `database/sql` driver that treats a Google Spreadsheet as a database:
one tab per table, the header row as the schema.

```go
import _ "github.com/danielloader/google-sheets-sql"

db, _ := sql.Open("sheets", "sheets://<spreadsheetID>?credentials=/path/key.json")

rows, _ := db.Query(`
    SELECT d.label, count(*), avg(e.salary)
    FROM employees e JOIN depts d ON e.dept = d.dept
    GROUP BY d.label HAVING count(*) > 1
    ORDER BY avg(e.salary) DESC`)
```

Joins, `HAVING`, aggregates and filtering are all evaluated **by Google**, not
by fetching the sheet and scanning it locally.

## How it works

Google exposes more than one engine that can execute a query over a
spreadsheet. This driver compiles SQL onto two of them and picks per statement.

**1. The Visualization query language (gviz).** A single-tab `SELECT` becomes a
`?tq=` request. Google evaluates it and returns only matching rows. No write
access needed, and it does not consume Sheets API read quota.

```
SELECT Major, count(*) FROM `Class Data` WHERE Gender = 'Female'
GROUP BY Major ORDER BY count(*) DESC LIMIT 4

  ->  select E, count(A) where B = 'Female' group by E order by count(A) desc limit 4
```

**2. The spreadsheet formula engine.** gviz cannot join, express `HAVING`,
`UNION` or `CASE`, and knows only a handful of functions. Anything needing more
is compiled into a single formula, written to a hidden scratch tab, evaluated by
Google, and read back. The scratch cell is always cleared afterwards.

```
SELECT e.name, d.label FROM employees e JOIN depts d ON e.dept = d.dept

  ->  =LET(_s0,FILTER(employees!A2:G,LEN(employees!A2:A)>0),
           _l1,{depts!A2:A,depts!A2:C},
           _s1,{_s0,ARRAYFORMULA(IFNA(VLOOKUP(INDEX(_s0,0,3),_l1,{2,3,4},FALSE),""))},
           _f,FILTER(_s1,ISNUMBER(MATCH(INDEX(_s1,0,3),depts!A2:A,0))),
           _r,QUERY(_f,"select Col2, Col9 label Col2 '', Col9 ''",0),
           ARRAYFORMULA(IF(_r="","␀",_r)))
```

Expressions the query language cannot evaluate — `CASE`, or a function outside
the gviz set — are computed *first* as extra columns of the source array and
then referenced by position, so the two engines cooperate within one statement:

```
SELECT upper(name), length(name) FROM employees

  ->  =LET(_s0,FILTER(employees!A2:G,LEN(employees!A2:A)>0),
           _x1,ARRAYFORMULA(LEN(INDEX(_s0,0,2))),
           _e,{_s0,_x1},
           _r,QUERY(_e,"select upper(Col2), Col8 ...",0), ...)
```

`upper()` stays inside `QUERY` because gviz has it; `length()` becomes a
computed column because it does not.

Set `SHEETSQL_DEBUG=1` to print the translated query or formula per statement.

## What runs where

| Feature | Where it runs | Notes |
| --- | --- | --- |
| `SELECT`, column lists, `*`, aliases | **Google** | `*` expands to labelled columns |
| `WHERE` (`= != < <= > >=`, `AND/OR/NOT`) | **Google** | |
| `LIKE`, `REGEXP` | **Google** | `REGEXP` maps to gviz `matches` |
| `IN`, `NOT IN`, `BETWEEN` | **Google** | expanded to `OR`/`AND` chains |
| `IS [NOT] NULL` | **Google** | |
| `GROUP BY` + `sum avg count min max` | **Google** | |
| `SELECT DISTINCT` | **Google** | rewritten as `GROUP BY` |
| `ORDER BY`, `LIMIT`, `OFFSET` | **Google** | including ordering by an aggregate |
| arithmetic `+ - * /` | **Google** | |
| `?` placeholders | **Google** | typed against the target column |
| date/string functions | **Google** | see below |
| **`INNER JOIN`, `LEFT JOIN`** | **Google** | formula engine; equality conditions, single or composite |
| **`HAVING`** | **Google** | second `QUERY` wrapping the grouped result |
| **`UNION`, `UNION ALL`** | **Google** | branches stacked with `{a; b}`; `UNION` wraps in `UNIQUE` |
| **`CASE WHEN`** (simple and searched) | **Google** | computed column via `IFS` |
| **extended scalar functions** | **Google** | computed columns, see below |
| `INSERT` | Google (write) | one `values.append` |
| `UPDATE`, `DELETE` row matching | **client** | see *Writes* below |

**Evaluated inside `QUERY` (no extra column):** `YEAR`, `MONTH`,
`DAY`/`DAYOFMONTH`, `HOUR`, `MINUTE`, `SECOND`, `QUARTER`, `DAYOFWEEK`,
`UPPER`/`UCASE`, `LOWER`/`LCASE`, `DATE`, `NOW`, `DATEDIFF`. `MONTH()` is
corrected to SQL's 1-based numbering; gviz counts months from zero.

**Computed as an extra column:** `ABS`, `SQRT`, `POWER`/`POW`, `FLOOR`,
`CEIL`/`CEILING`, `ROUND`, `MOD`, `EXP`, `LN`, `LOG10`, `SIGN`, `LENGTH`/
`CHAR_LENGTH`, `CONCAT`, `SUBSTR`/`SUBSTRING`, `LEFT`, `RIGHT`, `REPLACE`,
`TRIM`, `COALESCE`/`IFNULL`, `NULLIF`, `IF`, `CURDATE`.

> **Keep a computed column's types consistent.** `QUERY` infers one type per
> column and nulls values that do not fit, so `coalesce(salary, 'unset')`
> returns `NULL` rather than the string — the column is mostly numbers.
> `coalesce(salary, 0)` behaves as expected.

### Not supported

`RIGHT JOIN`, `FULL JOIN`, `CROSS JOIN`, `NATURAL JOIN`, `USING`, join
conditions other than equality, subqueries, window functions, mixing `UNION`
with `UNION ALL` in one statement, and scalar functions outside the lists above.
Each is rejected with an explicit error rather than silently ignored.

Transactions are refused rather than faked. A spreadsheet offers no isolation,
and a `Tx` that silently provided none would be worse than none at all.
`DELETE` is nonetheless atomic: `spreadsheets.batchUpdate` applies all of its
requests or none.

`LastInsertId` returns an error — a sheet has no primary key.

## Size limits

Measured against a live spreadsheet, three joinable tabs, correctness verified
against an independently computed expected count at every size.

| Rows per table | Single-table query | 3-table join | Result |
| --- | --- | --- | --- |
| 1,000 | 1.6 s | 2.2 s | correct |
| 5,000 | 0.9 s | 10.8 s | correct |
| 10,000 | 0.9 s | 37.5 s | correct |
| 25,000 | 1.1 s | — | join did not return |

**Recommended ceilings**

- **Single-table queries: comfortable to 25,000+ rows.** Cost is dominated by
  round-trip latency, not sheet size, because Google does the filtering.
- **Joins: up to ~10,000 rows per table.** `VLOOKUP` probes each row against
  the whole lookup column, so join cost grows faster than linearly. 5,000 is
  the comfortable working size; 10,000 works but takes ~40 s.
- **Keep the whole spreadsheet under ~2M cells.**

That last point is the one that bites hardest, and it is not obvious:

> A new tab is created with **26 columns** regardless of how many you use. Three
> 100,000-row tabs therefore occupy ~7.8M of the spreadsheet's 10M cell budget.
> At that size *every* request slows to tens of seconds — including reads of a
> different, 8-row tab in the same document, and including tab deletion. Size
> grids to their data (`AddSheet` with explicit `GridProperties`).

Two further platform facts worth knowing:

- **gviz parses the whole spreadsheet per request.** Once any tab is large,
  gviz is slow for every tab. Schema discovery therefore uses the Sheets API,
  not gviz.
- **Sheets API quota is 60 requests/minute/user.** The driver applies its own
  token bucket, shared process-wide per spreadsheet, because `database/sql`'s
  pool bounds concurrency but not request rate over stateless HTTP. Requests
  rejected with `429` are retried with exponential backoff, honouring
  `Retry-After`. A `5xx` is retried only for reads: a write that failed
  server-side may already have applied, and replaying it would duplicate rows.

## Row identity and concurrency

Give a tab a column headed `_rid` and the driver manages it as a stable row
identity. `INSERT` assigns the next value (`max(_rid)+1`, computed server-side).
The column is hidden from `SELECT *` and rejected as an assignment target.

With `_rid` present, `UPDATE` and `DELETE` re-read the tab immediately before
writing and re-locate every matched row by identity rather than position:

* a row that **moved** because someone inserted or deleted above it is still
  written correctly;
* a row **modified** or **deleted** since the scan aborts with a
  `*ConflictError` and nothing is applied;
* duplicate `_rid` values abort rather than guess.

Without `_rid`, writes are position-based and a concurrent edit between the
scan and the write can land on the wrong row.

This is optimistic concurrency: the window is narrowed, not closed. Sheets has
no compare-and-set primitive.

**A Drive revision pre-check does not work here.** Skipping the verification
re-read when the file looks unchanged would be a large saving, but Drive
reports a spreadsheet's `version` and `modifiedTime` lazily — both were measured
unchanged for more than 11 seconds after a committed Sheets write, and
Google-native files never populate `headRevisionId`. Trusting either would
report "nothing moved" while rows had moved. The re-read is unconditional.

## Concurrency model

Two different models apply, because the two engines have different constraints.

**Formula evaluation is single-writer**, like SQLite's writer lock. A formula
must occupy a real cell while Google computes it, so joins, `HAVING`, `UNION`
and `CASE` queries are serialised on the scratch sheet: one at a time per
`(spreadsheet, scratch sheet)`, process-wide. `database/sql` opens a connection
per concurrent query, so this lock deliberately sits outside the connection —
sharing a scratch cell would let two queries each read the other's result, with
no error raised.

Two consequences:

- Concurrent join queries queue rather than run in parallel. Given the
  60 requests/minute quota, request rate is usually the binding constraint
  anyway.
- The lock is **process-local**. Two processes querying one spreadsheet will
  collide on the same scratch cell. Give each a distinct `scratch=` tab.

**Single-tab reads are unsynchronised.** They go straight to gviz, hold no
state in the document, and may run fully in parallel.

**Row writes are optimistic, not serialised.** `INSERT` appends. `UPDATE` and
`DELETE` re-verify their target rows immediately before writing and abort with
a `*ConflictError` if anything moved (see below), rather than taking a lock.
Concurrent writers therefore make progress; a loser is told, not blocked.

## Writes

| Statement | Implementation |
| --- | --- |
| `INSERT` | `values.append`, one call |
| `UPDATE` | full read to locate rows, then `values.batchUpdate` |
| `DELETE` | full read to locate rows, then an atomic `spreadsheets.batchUpdate` |

`UPDATE` and `DELETE` cannot push their `WHERE` clause to Google: neither engine
reports row numbers, and those statements need them to address cells. The
predicate is evaluated locally against a full read of the tab.

## DSN

```
sheets://<spreadsheetID>[?param=value&...]
```

A pasted browser URL (`https://docs.google.com/spreadsheets/d/<id>/edit`) also
works.

| Parameter | Default | Meaning |
| --- | --- | --- |
| `credentials` | ADC | path to a service account JSON key |
| `sheet` | first tab | default tab when a query names none |
| `header` | `1` | header rows; `0` addresses columns as `A`, `B`, `C` |
| `scratch` | `_sheetsql_scratch` | hidden tab used to evaluate formulas |
| `timeout` | `180s` | per request |
| `rate` | `60` | requests/minute budget |
| `max_rows` | `50000` | ceiling on rows read back from a formula |
| `readonly` | `false` | reject writes, and with them the join engine |

## Access

The spreadsheet must be shared with the service account in the key file —
Editor for writes and for joins (the scratch tab is a write), Viewer for
gviz-only reads.

Service accounts have no Drive storage quota and **cannot create spreadsheets**.
Create the sheet as a human and share it.

## Testing

Unit tests cover translation, formula compilation, local evaluation and value
conversion, and need no network:

```
go test ./...
```

Live tests run against a real spreadsheet:

```
go run ./cmd/bootstrap -id <spreadsheetID>       # load fixtures
SHEETSQL_DSN="sheets://<id>?credentials=/path/key.json" go test -count=1 .
```

Tools:

- `cmd/sheetsql` — run one query, print the result and the translation
- `cmd/bootstrap` — load test fixtures (`-scale N`, `-big N` for large tabs)
- `cmd/bench` — latency of reads and writes against a loaded sheet
- `cmd/scaletest` — reproduce the size table above
- `cmd/dropsheets` — delete tabs and report the cell budget

## Status

Prototype, exercised end to end against a live spreadsheet: 77 tests covering
typed reads, `NULL` handling, date filtering, server-side aggregation, scalar
pushdown, `CASE`, `UNION` and `UNION ALL`, two- and three-table joins,
composite join keys, `LEFT JOIN` semantics, `HAVING`, row-identity retargeting
under concurrent edits, conflict detection, and an insert/update/delete round
trip, plus offline coverage of the retry and rate-limiting behaviour.

## Licence

MIT
