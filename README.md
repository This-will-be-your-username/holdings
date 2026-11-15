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
  and sales recorded on or after 1 July 2027 (the Tax page still applies the current rules to them; use the Sell estimator to compare). Estimates only, not tax advice.

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

## CGT before vs after 1 July 2027
The CGT reform (Treasury Laws Amendment (Tax Reform No. 1) Act 2026, Royal Assent 26 June 2026) replaces the 50%
discount from 1 July 2027. Both the **Sell estimator** and the **Withdraw an amount** planner now price the same sale
under both sets of rules side by side:
- **Current rules** (sell by 30 June 2027): 50% discount on parcels held over 12 months.
- **New rules** (sell on/after 1 July 2027): each holding is treated as re-acquired at its value just before
  1 July 2027 - assumed here to be **today's price** ("the new cost base sits at the current value"). The gain built up
  to then is deferred and keeps the 50% discount; growth after it is taxed on a CPI-indexed basis (indexation can't create
  or enlarge a loss) with a **30% minimum tax** top-up on that growth. Capital losses are applied to the deferred gains first.
Assumptions you can change (a collapsible panel on both forms): sale date under the new rules, price growth after
1 July 2027 (% a year, default 0 - with no growth the two regimes cost the same; the gap appears as soon as prices rise)
and CPI (default 2.5%). Not modelled: the minimum-tax exemption for income-support recipients, trusts, valuation by the
ATO's apportionment method, and how the 12-month test works after the reset (original purchase date is assumed).

## Live prices and saved history
- **Live while the market is open.** For every listed holding the app polls a small quote every 30 seconds during the
  exchange's regular hours (ASX 10:00-16:10 Sydney, US 09:30-16:00 New York; weekends and public holidays are detected
  from the session window Yahoo returns with each quote). The dashboard shows a status chip ("Live - ASX open - updated
  10:42:05" / "Markets closed - prices as at Fri 13 Nov") and refreshes its numbers by itself. Nothing is fetched while a
  market is closed.
- **Sources.** Yahoo Finance for ASX and US prices; if Yahoo rate-limits or fails, ASX symbols fall back to the ASX's own
  bid/ask midpoint (only while trading). A 429 makes the app back off instead of hammering Yahoo.
- **History is saved, not re-downloaded.** Daily prices live in the data file and are only topped up (the last ~10 days)
  on each refresh. Benchmarks and FX are incremental too; secondary lookups are throttled to a few times a day. Live quotes
  are kept in a small side file (`<data file>.quotes`) so the main file isn't rewritten every few seconds.
- **No more silent failures.** The old refresh stamped "last refreshed" even when every request failed. Now that time only
  moves when something really updated, and any failure (rate limit, bad symbol, Vanguard unreachable) is saved and shown
  on the dashboard.

## Vanguard unlisted funds (Fund prices page)
Unit prices come straight from Vanguard Australia's own site API - the data behind each fund's *Prices and distributions*
tab: `GET /personal/api/products/personal/funds/allfunds` (fund list with portId and APIR) and
`GET /personal/api/products/personal/fund/{portId}/prices` (daily NAV history). Hold the fund under its APIR code
(e.g. `VAN0108AU`, market *Managed fund*) and the portId is found automatically (VAN0108AU -> 8121); you can also set it by
hand from the fund page's `portId=` address. Prices are fetched on refresh (at most a few times a day) or with
**Fetch from Vanguard now**. Fallback: **Import price history** reads Vanguard's export (`.xlsx` or CSV with
`Date, Buy, Sell, NAV`, or any date/price table; NAV preferred, then unit price, then sell, then buy). Imported prices
override fetched ones on the same date. The transaction importer now accepts `.xlsx` files too.

## Reconciling with your provider

If FundTrack's total differs from your provider's, open **/reconcile** (link on the Transactions page) and upload the provider's holdings export (CSV or xlsx). It compares units per holding and shows which are short and by how much. It changes nothing: the usual cause is a transaction export that doesn't go back far enough, so import the earlier history (duplicates are skipped by fingerprint) and compare again. FundTrack also warns on the dashboard when a holding's first record is a reinvestment, which usually means earlier purchases are missing. Cash balances are not tracked.


## Private companies

Shares in an unlisted company are tracked on the **Private** page. Add the company, record your shares as normal buy/sell transactions (market: Private company), and record **value checkpoints** - a date plus either a price per share or the company's reported valuation (divided by the shares on issue). The holding is valued at its latest checkpoint and flagged on the dashboard when that is over a year old. **Search for a valuation** looks for reported valuations in public web results (only the company name is sent); results are shown with their source and are never saved unless you choose to record them. Private holdings are never suggested by the sell estimator.

## Currency conversion

Foreign holdings are converted to AUD using ECB rates from Frankfurter. Each refresh also pulls the newest published rate (not just history), including for unlisted and private holdings in foreign currencies. AUD is shown first with the original amount in brackets, e.g. `3,200.00 (USD 2,000.00)`; the rate and its date are shown under the holdings table, and a warning appears if the latest rate is over a week old.

## Chart explorer

The **Charts** page plots your total value or any mix of investments (optionally combined into one line, with money invested overlaid). Metrics: value, gain/loss (value minus net invested) and a price index rebased to 100 so investments compare fairly. Pick a common range (1M, 3M, 6M, YTD, financial year, 1Y, 3Y, 5Y, All) or enter exact dates; drag across the chart to highlight a period, see its start, end and change per line, and zoom in on it. Your selection is remembered in the browser.

Exchange rates are refreshed automatically when the last pull is over 24 hours old (checked every 30 minutes), and can be refreshed on demand with the button on the Fund prices page. A failed pull doesn't reset the 24-hour clock.

## RSUs and awarded shares

Import actions such as *Release*, *Vest* or *Award* are recognised as **shares awarded**, not purchases. Units are added at the market value on the vest date, which becomes the cost base and the date the 12-month CGT discount clock starts. For performance they are treated as invested at that value, so gains are measured from vest. Sell-to-cover rows import as ordinary sales.

## Sell everything

The **Sell everything** tab on the Sell estimator prices selling every holding at once, with the gain taxed at the top marginal rate (47% including Medicare) whatever your own income is. It splits gains into long-term (over 12 months, 50% discount) and short-term, applies losses, and shows the tax owed and the cash you would keep.

Private companies can be tracked in any currency: pick it when you add the company, or in the Currency box when you first record shares for it on the Transactions page. Prices, checkpoints and cost are kept in that currency; values are reported in AUD at the latest saved exchange rate (and at the day's rate for each purchase), with the original amount in brackets.
