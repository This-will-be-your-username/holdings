# Architecture

Holdings is a single Go program using only the standard library. It serves server-rendered HTML pages (`html/template`) with small amounts of embedded JavaScript, and keeps all data in a JSON file.

```
browser  <->  net/http routes  <->  compute (pure functions)  <->  Store (JSON file)
                    |                                                 |
                    +--> price feeds (Yahoo, ASX, Vanguard, ECB)  ----+
```

## Principles

- **Computation is pure.** `Compute` and `ComputeTax` take a data snapshot and return results without writing anything. Hypothetical sales and per-owner views work by building a shallow copy of the data with extra transactions or different settings.
- **One file, one lock.** The `Store` holds the data in memory behind a read-write lock and writes the JSON file after each change.
- **No dependencies.** Even the xlsx reader is written against the standard library.
- **Assets are embedded** (`web/` via `embed`), so the binary is self-contained.
- **Honest failures.** Fetch errors are recorded and shown; a stale price is flagged rather than hidden.

## Source layout

### Core

| File | Responsibility |
|---|---|
| `main.go` | Startup, routes, template functions, add/delete transaction handlers. |
| `store.go` | Data types (`DB`, `Instrument`, `Txn`, `Settings`), the JSON store, side file for live quotes. |
| `perf.go` | `Compute`: replays transactions into holdings, cost, income, realised/unrealised gains, XIRR, warnings and the value chart. |
| `tax.go` | Parcel replay (`cgtReplay`), CGT events, income-year tax estimate, tax rates, exports. |
| `owners.go`, `ownerpages.go` | Owner model, per-owner views and settings, the owner management handlers. |
| `export.go` | CSV export, transaction validation, parsing helpers. |
| `fingerprint.go` | Transaction fingerprints and duplicate classification. |

### Prices and data feeds

| File | Responsibility |
|---|---|
| `market.go` | Yahoo chart client, exchange-rate fetch, instrument resolution. |
| `live.go` | Live quote polling during market hours, status for the dashboard. |
| `refresh.go` | Background refresh of history (incremental), ASX details, Vanguard prices, benchmarks. |
| `telegram.go`, `sparkline.go`, `settingspage.go` | Telegram bot (long polling, commands, snapshot), PNG graph drawing, and the Settings page. |
| `fx.go` | Exchange-rate refresh on its own 24-hour schedule. |
| `vanguard.go`, `priceimport.go`, `xlsx.go` | Vanguard API client, price-history file import, xlsx reader. |
| `pricelookup.go` | Closing-price lookup for manual transactions. |
| `pricespages.go` | Fund prices page handlers and the dashboard live view. |

### Importing

| File | Responsibility |
|---|---|
| `importer.go` | Delimiter/header detection, column scoring, date/number/ticker/action parsing, transfer and RSU handling. |
| `importpages.go` | Review page, commit and undo handlers. |
| `reconcile.go` | Provider holdings parsing, matching, reconciliation page. |

### Selling and planning

| File | Responsibility |
|---|---|
| `estimate.go`, `estowner.go` | Sale estimator core and the owner-aware wrapper that splits a sale by owner and merges the results. |
| `sellall.go` | Sell everything, tax-rate assumption (`withBracket`). |
| `withdraw.go`, `plan.go` | Withdrawal planner, target allocation and weighting. |
| `cgtnew.go` | The 1 July 2027 CGT comparison. |
| `outlook.go`, `outlookpage.go` | Six-month projection (bootstrap simulation) and its page. |
| `chartdata.go` | Aligned data set for the chart explorer. |

### Other

| File | Responsibility |
|---|---|
| `private.go` | Private companies: checkpoints, valuation search, handlers. |
| `txnedit.go` | Editing recorded transactions. |
| `testdata.go`, `testmode.go` | The simulated data set and the test/real switch. |
| `taxpages.go` | Tax page, settings and distributions page. |
| `gzip.go` | Response compression. |

### Web

`web/*.html` are the page templates (a `base.html` layout plus one file per page). `web/static/` holds the stylesheets (`style.css`, `app.css`) and scripts: `charts.js` (SVG charts for the dashboard and outlook), `explorer.js` (the chart explorer) and `live.js` (dashboard auto-refresh).

## Request flow example: estimating a sale

1. `GET /estimator?run=1&sym=...` is handled by `estimatorPage`.
2. A tax-rate choice is applied by `withBracket` (a copy of the data with every owner's income set to the top bracket, if chosen).
3. `EstimateSale` groups the requested sales by owner. For each owner it builds `ownerView(owner)` and calls `estimateCore`.
4. `estimateCore` appends hypothetical sells (IDs starting `est-`) to the owner's transactions, runs `Compute` and `ComputeTax` for the income year with and without them, and reports the difference.
5. `mergeEstimates` adds the owners' results together. `CompareNewLawAll` prices the same sale under the 2027 rules.
6. The template renders tiles, tables and the per-owner breakdown.

## Testing

`go test ./...` runs unit and handler tests. They use synthetic data and fake servers (`httptest`) for Yahoo, the ASX, Vanguard, the exchange-rate service and the search service, an injectable clock for market hours, and a smoke test that renders every page. Nothing in the tests uses real data or the real network.

When adding a feature, put the calculation in a pure function, give it a test with hand-checked numbers, and add the page to the smoke test.

## Licence

GPL-3.0. See `LICENSE` in the repository root.

## Contributing notes

- Keep it dependency-free.
- Never commit anything under `data/`, and never put real names, account numbers or figures from your own records in tests or docs. Use invented values.
- Rebuild after changing anything under `web/`.
