# Getting Started

## Requirements

- [Go](https://go.dev/dl/) 1.22 or newer. There are no other dependencies: the app uses only the Go standard library.
- A web browser.
- Internet access if you want live prices. The app works offline with data you enter yourself.

## Run it

```bash
go run .
```

Open <http://127.0.0.1:8080>.

To build a single executable (the web pages, styles and scripts are embedded in it):

```bash
go build -o fundtrack .
./fundtrack
```

If you change anything under `web/`, rebuild: the assets are baked into the binary.

Prefer not to install Go? See [Running with Docker](Running-with-Docker.md).

## Settings (environment variables)

| Variable | Default | Meaning |
|---|---|---|
| `ADDR` | `127.0.0.1:8080` | Address and port to listen on. The default only accepts connections from your own machine. |
| `DATA_FILE` | `data/fundtrack.json` | Where your data is stored. |
| `TEST_MODE` | unset | `1` starts on simulated test data, `0` forces your real data. See [Test Mode](Test-Mode.md). |

```bash
ADDR=127.0.0.1:9000 DATA_FILE=./data/me.json go run .
```

> **Security note.** The app has no login. Keep the default address so only you can reach it. If you set `ADDR` to something like `:8080` it becomes reachable from your network and anyone who can reach it can read and change your data.

## First five minutes

1. **Try it with test data.** Flip the **Test data** switch in the header. You get a realistic portfolio of simulated investments (VGS, VDHG, VDGR and an unlisted Vanguard fund) so you can look at every page without entering anything. Your real records are never touched while the switch is on. See [Test Mode](Test-Mode.md).
2. **Switch it off** and add your own data in one of two ways:
   - Add a few transactions by hand on the **Transactions** page ([Transactions](Transactions.md)).
   - Upload a CSV or xlsx from your broker or fund manager ([Importing Transactions](Importing-Transactions.md)).
3. **Check prices.** Listed holdings are priced automatically. For Vanguard unlisted funds see [Prices and Exchange Rates](Prices-and-Exchange-Rates.md).
4. **Set your tax assumptions** on the **Tax** page (other taxable income, parcel method). Without an income the estimators assume the top tax bracket. See [Tax and CGT](Tax-and-CGT.md).
5. **Compare with your provider** if the total doesn't match what you see at your broker: [Reconcile](Reconcile.md).

## Where things are

| Path | Contents |
|---|---|
| `data/fundtrack.json` | Your data: transactions, instruments, prices, settings, owners. |
| `data/fundtrack.json.quotes` | Live quotes, kept separately because they change every few seconds. |
| `data/fundtrack-test.json` | The simulated test data. |
| `data/mode` | Remembers whether test mode is on. |

The `data/` directory is excluded from git. See [Configuration and Data](Configuration-and-Data.md) for backups and privacy.

## Running the tests

```bash
go test ./...
```

The tests use fake price feeds and synthetic data. They don't touch the network or your real data.
