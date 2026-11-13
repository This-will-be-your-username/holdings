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
