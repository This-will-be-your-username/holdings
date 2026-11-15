# Performance Calculations

FundTrack measures **your** return: it replays your transactions in date order and works out cost, value, income and money-weighted return from what you actually did.

## Replaying transactions

Transactions are sorted by date (buys and awards, then reinvestments, then distributions, then sells on the same day). For each holding the app tracks:

- **Units** held.
- **Cost** (in AUD): price × units × exchange rate on the day, plus brokerage. Foreign trades use the rate on the transaction date (or the rate you enter).
- **Average cost** = cost ÷ units. A sell removes cost proportionally, so average cost stays the same after a sale.

## Return measures

| Figure | How it is worked out |
|---|---|
| **Market value** | Units × latest price × latest exchange rate. |
| **Unrealised gain** | Market value − remaining cost. |
| **Realised gain** | Sale proceeds (net of brokerage) − the average cost of the units sold. |
| **Income** | Distributions and dividends received, converted to AUD, net of any foreign tax withheld. |
| **Franking credits** | Added to the total return unless you turn them off in the Tax page assumptions. |
| **Total return** | Unrealised + realised + income (+ franking). The percentage is on money invested. |
| **XIRR** | The annual rate that makes the net present value of your dated cash flows zero, so timing matters. See below. |

Note that the performance figures use an **average-cost** basis. Tax calculations use **parcels** (each purchase separately), see [Tax and CGT](Tax-and-CGT.md).

## Cash flows and XIRR

XIRR uses dated cash flows from your point of view:

- A buy or an award is money invested (negative).
- A sell is money returned (positive).
- A cash distribution is positive.
- The current market value is the final positive flow on today's date.
- A reinvested distribution (DRP) is income and a new purchase at the same time, so it has no net cash movement.

XIRR is **money-weighted**: it answers "what annual rate did *my* money earn, given when I added to it?" It will differ from a fund's published (time-weighted) return.

## Special cases

- **RSU awards** are counted as invested at their market value on the vest date, so gains are measured from vest.
- **Private companies** are valued from your value checkpoints, carried forward between them. Nothing is estimated between checkpoints. See [Private Companies](Private-Companies.md).
- **Foreign holdings** are valued at the latest exchange rate. The currency effect is therefore part of the return.
- **Unlisted funds** use the unit price (NAV) from Vanguard or your entered prices.
- **No price**: a holding with no market price is valued at your last trade price and flagged.
- **Missing history**: if a holding's first record is a reinvestment, the dashboard warns that earlier purchases are probably missing, which understates units and value. Use [Reconcile](Reconcile.md).

## Money invested

The **Invested** column and the dashed line on charts are cumulative money put in, net of sales. In the holdings table, *Invested* is all buys including brokerage (not netted for sales). RSU awards count at their vest value even though no cash left your account.

## Not modelled

Stock splits recorded as transactions (prices are split-adjusted by Yahoo; for unit counts, enter them as your broker reports), corporate actions such as mergers and spin-offs, and cash balances. See [Limitations and FAQ](Limitations-and-FAQ.md).
