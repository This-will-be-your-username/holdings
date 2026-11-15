# Limitations and FAQ

## What isn't modelled

**Tax**

- Medicare levy low-income reduction and surcharge, HELP/HECS, deductions, and offsets other than franking credits and the foreign income tax offset.
- Companies, trusts and SMSFs. Owners are treated as resident individuals.
- Joint holdings (each investment has exactly one owner).
- Wash-sale and trading-stock rules, foreign-currency hedging, and non-resident or part-year residency.
- For the 2027 CGT reform: the minimum-tax exemption for income-support recipients, the ATO's apportionment valuation, and how the 12-month test works after the reset.
- Sales dated on or after 1 July 2027 on the Tax page use the current rules; use the [Sell Estimator](Sell-Estimator.md) for the comparison.

**Investments**

- Stock splits and other corporate actions as transactions (prices from Yahoo are split-adjusted, so enter units as your broker reports them), mergers and spin-offs.
- Cash balances.
- Options, derivatives and cryptocurrency.
- Distribution timing beyond the income-year assignment you provide.

**Data**

- Prices come from unofficial free feeds that can change or limit access.

## FAQ

**Why does my total differ from my broker's?**
Usually because the transaction history you imported doesn't go back far enough. Upload your provider's holdings export on the [Reconcile](Reconcile.md) page and import the earlier history. Cash balances and price dates also cause small differences.

**Why is the tax estimate so low?**
If your other taxable income is $0 on the Tax page, a sale is taxed as your only income, so the first $18,200 is tax-free. Set your income, or choose the top bracket in the [Sell Estimator](Sell-Estimator.md).

**Can I use it for several people?**
Yes: [Owners](Owners.md). Each owner has their own income, losses and parcel method, and a sale across owners is taxed separately for each.

**How do I record shares I received from my employer?**
Use type *Shares awarded (RSU)* on the [Transactions](Transactions.md) page, or import rows with *Release*/*Vest*/*Award* wording. For private-company shares where tax is deferred, see [Private Companies](Private-Companies.md).

**I only know the total cost of an old holding, and not when I bought it.**
Enter the total with the *total* box ticked and leave the acquisition date blank. Tick *held over 12 months* only if you're sure, to get the CGT discount. See [Transactions](Transactions.md).

**Can I edit a transaction I imported?**
Yes, with the pencil icon in the history. The fingerprint is kept, so re-importing the same row doesn't bring the old figures back.

**How do I undo an import?**
*Recent imports* on the Transactions page has an **Undo** button for each import.

**Where is my data, and how do I back it up?**
In `data/fundtrack.json` by default. Copy the file. See [Configuration and Data](Configuration-and-Data.md).

**Can I run it on a server for my phone?**
You can set `ADDR`, but there is no login. Put it behind your own authenticated reverse proxy or VPN. The layout already works well on phones.

**Why are the Outlook ranges wide?**
Because markets are uncertain. The bands are a range of plausible outcomes from your holdings' own volatility, not a forecast. See [Outlook](Outlook.md).

**Is it tax advice?**
No. It produces estimates for your own records. Check anything that matters with the ATO or a registered tax agent.
