# Configuration and Data

## Settings

| Variable | Default | Meaning |
|---|---|---|
| `ADDR` | `127.0.0.1:8080` | Listen address. The default accepts connections only from the same machine. |
| `DATA_FILE` | `data/fundtrack.json` | Path of the data file. Other files are created beside it. |
| `TEST_MODE` | unset | `1` start in [Test Mode](Test-Mode.md), `0` force real data. |

Tax assumptions (other income, parcel method, losses brought forward) and owners are stored in the data file and edited on the [Tax](Tax-and-CGT.md) page.

## Files

| File | Contents |
|---|---|
| `<data file>` (JSON) | Transactions, instruments, saved prices, manual prices, distributions, exchange rates, import log, settings, owners, private-company checkpoints. |
| `<data file>.quotes` | The latest live quotes. Kept separately because they change every few seconds. |
| `fundtrack-test.json` (same folder) | The simulated test data. |
| `mode` (same folder) | Remembers whether test mode is on. |

The file is plain JSON. It is written atomically each time you change something, so a crash doesn't leave a half-written file.

## Backups

Copy the data file. That's all of your data. To restore, put it back and restart. You can also use **Export transactions CSV** on the dashboard as a portable backup of your transactions: the app's own export re-imports as 100% already recorded, and as new transactions into an empty database.

To start fresh, stop the app and move the data file aside.

## Privacy and security

- **Your data stays on your machine.** There is no account, no telemetry and no cloud sync.
- **No login.** Anyone who can reach the address can read and change your data. Keep `ADDR` on the loopback address unless you put your own authentication in front of it.
- **The data file is not encrypted** and is created with ordinary file permissions. Use disk encryption and restrict the folder if the machine is shared.
- The `data/` folder and executables are excluded from git by `.gitignore`. Don't commit your data file.
- Uploaded files are held in memory for the duration of the import session, not written to disk. Reconcile ignores and never stores account names and numbers.

### What the app contacts

All requests are made by the app on your behalf, and none needs an account or key:

| Service | When | What is sent |
|---|---|---|
| Yahoo Finance | Price refresh, live quotes, price lookup for a new transaction, outlook benchmarks | Ticker symbols and date ranges |
| ASX public API | ASX distribution details | ASX codes |
| Vanguard Australia | Unit prices for unlisted Vanguard funds | The fund's id |
| Frankfurter (ECB rates) | Exchange rates | Currency codes and dates |
| DuckDuckGo (HTML) | Only when you press *Search for a valuation* on a private company | The company name |

No transaction amounts, units, balances or personal details are sent anywhere. In [Test Mode](Test-Mode.md) price fetching and the valuation search are switched off.

## Upgrading

Pull the new code and rebuild. The data file format is extended in a backwards-compatible way (new fields are optional), so existing data keeps working. Take a copy of the data file before upgrading as a precaution.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| Prices stuck, a red note on the dashboard | A feed failed or was rate-limited. The note says which. Try *Refresh prices* later. |
| A holding is valued at your last trade price | No market price could be found. Check the symbol, or for unlisted funds enter or import prices on [Fund prices](Prices-and-Exchange-Rates.md). |
| An exchange rate warning | The latest rate is over a week old. Press *Refresh exchange rates now* on the Fund prices page. |
| Total is lower than your provider's | Earlier history wasn't imported. Use [Reconcile](Reconcile.md). |
| Changes under `web/` don't appear | The assets are embedded in the binary. Rebuild. |
