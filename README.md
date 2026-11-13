# FundTrack

Personal investment performance tracker (Go, stdlib only). Track ASX ETFs, Vanguard
funds and US stocks; export a CSV of your transactions with AUD values, cost basis and
realised gains.

    go run .            # http://127.0.0.1:8080
    ADDR=:9000 DATA_FILE=./data/me.json go run .

## Data sources (all keyless)
| Data | Source |
|---|---|
| ASX + US prices, split-adjusted daily closes, ex-dividend dates | Yahoo Finance chart API (`query1.finance.yahoo.com/v8/finance/chart/{SYMBOL}`; ASX = `.AX`) — unofficial |
| AUD FX (US holdings) | Frankfurter / ECB (`api.frankfurter.dev`) |
| Vanguard unlisted managed funds | No public API — enter unit prices on the *Fund prices* page |

## Performance maths
Average-cost basis incl. brokerage, FX at transaction date, income tracked separately,
XIRR (money-weighted) over your actual cash flows. DRP counts as income + purchase.
Not modelled: franking credits, US withholding tax, CGT parcel selection, splits.

Run `go test ./...` for the calculation tests.

## Tax (Australian resident individual)
- **Franking & distributions:** record gross distribution, franked amount, franking credits, foreign tax,
  capital-gain components and tax-deferred amounts per distribution. For ASX holdings the latest
  distribution's pay date and franking % are pulled from the ASX (`asx.api.markitdigital.com`) to pre-fill credits.
- **CGT:** parcel-based (FIFO / highest cost / lowest taxable gain), 12-month 50% discount, loss ordering,
  carry-forward, tax-deferred cost-base reduction (excess = event E4).
- **Estimate by income year (1 Jul–30 Jun):** marginal tax + 2% Medicare on investment income stacked on your
  other income, less franking and foreign tax offsets. Rates: 2025-26 16%, 2026-27 15%, 2027-28+ 14% first bracket.
- **Exports:** `/export/tax-summary.csv`, `/export/tax-distributions.csv?fy=2025-26`, `/export/tax-cgt.csv?fy=2025-26`.
- Not modelled: Medicare low-income reduction/surcharge, HELP, deductions, companies/trusts/SMSF/joint holdings,
  and the proposed post-1 July 2027 CGT indexation regime (not law at last check). Estimates only, not tax advice.

## Sell estimator (`/estimator`)
"What would selling these cost me in tax?" Add hypothetical sales (units, `all`, or a dollar target; optional
price, brokerage and date) and the app adds them to your real parcels and recomputes that income year, so existing
gains/losses, carried-forward losses, franking offsets and your other income are all accounted for.
- Compares FIFO / highest-cost / lowest-taxable-gain parcel matching for the same sale.
- Shows parcels sold, net proceeds, extra tax and after-tax proceeds, plus before/after for the year.
- Flags when waiting past the 12-month mark would cut the tax (assuming an unchanged price).
Shareable via the URL query string. Estimates only, not tax advice.

## Outlook (`/outlook`) — six-month projection
Interactive charts (hover or touch for values) of where your portfolio and each holding could be in ~6 months.
This is a **statistical projection, not a prediction**: it shows ranges (50% / 80%) with probabilities.
- **Past behaviour:** up to 5 years of daily history; each holding's own daily moves are bootstrapped (the same
  days across holdings, preserving correlation) around an assumed drift into thousands of possible paths.
- **Assumptions** (selector on the page): *Balanced* (avg of last-year growth, full-history growth and 6% long-run,
  capped, plus a ±1.5%/yr tilt when the market is clearly above/below its 200-day average), *Recent trend*,
  *Long-run*, *No growth*. Volatility is scaled by how choppy the last 60 days were vs the long run.
- **Wider market:** S&P/ASX 200, S&P 500, VIX and AUD/USD (Yahoo) with trend (50/200-day averages), returns,
  volatility, drawdown; per-holding beta/correlation vs the relevant index, 1-year regression trend line.
- Price-only (distributions not included); managed funds need ~120 manual unit prices to be modelled.

## Mobile & desktop
Responsive layout: scrollable nav, charts redrawn at the real screen width, tables collapse into labelled cards on
phones, 16px inputs and 44px touch targets, gzip for small payloads. Static assets are embedded in the binary —
rebuild after changing anything under `web/`.

## Withdraw an amount (`/estimator?mode=raise`) and target allocation (`/plan`)
Say how much cash you want out and the planner chooses what to sell, in this order of priority:
1. **Diversification.** Set what each holding is for on the **Target allocation** page (role, target % of the
   portfolio, intended horizon, expected return). The planner sells what is *overweight* and protects what is
   underweight, finishing as close to your target mix as it can (a least-squares rebalance solved exactly).
   With no targets set, your current mix is the target, so a sale is spread roughly in proportion.
2. **Performance judged against intent.** Each holding's return is compared with *its own* expected return, and
   that verdict is weighted by how much of its intended horizon has passed (a 10-year growth holding two years in
   counts at 20%; a short-term holding past its horizon counts in full). It only nudges the target, by at most
   10% of itself, so it can tip a close call but never override balance. Holdings with no purpose set are never
   judged on performance.
3. **Tax.** Within each holding: parcels held over 12 months first (50% CGT discount), lowest taxable gain first.
   A parcel under 12 months old is used only if avoiding it would leave you more than 2.5 points further from target.
Options: brokerage per holding, sale date, "after tax" (the amount is cash in hand after estimated CGT), restrict to
chosen holdings. A **lowest tax overall** plan is shown for comparison, with how far it leaves you from your target.
Listed securities sell in whole units, so the total can be slightly over. Each parcel shows why it was chosen.

## Importing broker / fund-manager CSVs
Upload any export (or paste CSV text) on the Transactions page. The importer needs no template:
- finds the delimiter and header row (skips statement preambles and total rows);
- scores every column from header-name regexes **and** the values (dates, tickers, buy/sell wording);
- reads dates (day-first / month-first / ISO / `2 Jan 2025` / `20250102;093015`), numbers (`$1,234.50`, `(12.5)`,
  `1.234,50`, `12.5CR`), actions (`Buy/Bought/B/Application`, `Sell/Sold/Redemption`, `Distribution`, `DRP`),
  tickers (`VAS`, `VAS.AX`, `ASX:VAS`, `VAS AU Equity`, `NASDAQ:AAPL`, APIR `VAN0002AU`, ISIN, `Name (VAS)`),
  signed quantities, brokerage + GST, currency and FX direction, franking and withholding columns;
- shows what it understood on a **review page** where every column choice, the date format and the default
  market can be changed before anything is saved.

## Fingerprints & duplicate detection
Every transaction gets a content **fingerprint** (hash of date, symbol, type, units, price - or amount for
distributions - ignoring brokerage/notes/formatting) and keeps any institution **reference** (contract note
number). On upload each row is classified:
- **already recorded** - same reference, or same fingerprint (count-aware: two identical trades on one day are
  two transactions, so only as many copies as you already have are skipped);
- **possible duplicate** - same trade with a slightly different date (trade vs settlement) or rounded price;
  held back unless you tick *import anyway*;
- **new**.
The same trade arriving from two different brokers' formats is recognised, an identical file re-upload is
flagged, the app's own export re-imports as 100% already recorded, and each import can be **undone**.

## Test mode (dummy data)
A **Test data** switch in the page header flips the whole app onto a separate, persistent set of simulated
investments built around **VGS, VDHG, VDGR and the unlisted VAN0110AU fund**. It is stored in its own file
(`data/fundtrack-test.json`) next to your real data, so edits persist across restarts like real data, and your
real records are never opened for writing while it is on. The choice is remembered (`data/mode`).

    TEST_MODE=1 go run .      # start in test mode   (TEST_MODE=0 forces real data)

What's in it (about 130 transactions over ~3 years, deterministic apart from dates moving with "today"):
- lump-sum and monthly/quarterly buys with brokerage; contract-note references on the first year (marked as a
  sample import so it can be undone);
- quarterly distributions with franking credits, foreign-tax offsets, discountable capital gain and tax-deferred
  amounts, VDHG reinvested (DRP), June-quarter payments assigned to the earlier income year;
- sells covering every CGT case: a discounted gain, a full-rate gain on a young parcel, and a capital loss;
- 5 years of daily prices driven by shared market factors (so betas/correlations make sense), market benchmarks,
  weekly unit prices for the managed fund, and the latest distribution left unrecorded so the "may be missing"
  suggestions appear. Price refresh is disabled (nothing is fetched in test mode).
The banner offers **Reset test data** (regenerate), a **sample broker CSV** and a **sample fund statement**, built from
the test data so you can try the importer's duplicate handling (already recorded / possible duplicate / new / skipped).

Test mode also seeds a target allocation (VGS 30%, VDHG 30%, VDGR 25%, VAN0110AU 15%) that deliberately differs
from the current mix, so a withdrawal has something to rebalance.
