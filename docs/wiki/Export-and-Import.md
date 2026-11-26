# Export and Import

Move everything from one installation to another (a new computer, from running the app directly to [Docker](Running-with-Docker.md), or between servers) with one file.

## What's in an export

An export is a single JSON file holding your whole data file:

- transactions, including their fingerprints and source references, so later broker imports still recognise duplicates
- instruments, with roles, targets, owners and other details
- fetched and manually entered prices, distributions and exchange rates
- the import log
- tax settings and [owners](Owners.md)
- [private-company](Private-Companies.md) value checkpoints

Optionally, the [Telegram bot](Telegram-Bot.md) token and approved chat. These are left out unless you ask, because the token is a secret.

Live quotes are not included. They're refetched within seconds.

The file is plain, readable JSON. It isn't encrypted, so keep it somewhere private, especially if it includes the Telegram token.

## From the app

Everything is on the **Settings** page, under **Export and import**.

**Export:** press **Export everything**. Tick *Include the Telegram bot token* first if you want the bot to work on the new installation without setting it up again. The browser downloads `holdings-backup-YYYY-MM-DD.json`.

**Import:** on the new installation, choose the file, tick *Replace my current data with this file* and press **Import**. Tick *Also restore Telegram settings* if the file has them and you want them.

Then press **Refresh prices** to fetch any prices newer than the export.

## From the command line

The same binary has `export` and `import` commands. They use the same `DATA_FILE` setting as the app (default `data/fundtrack.json`).

```bash
holdings export backup.json              # or: holdings export > backup.json
holdings export -telegram backup.json    # include the Telegram settings
holdings import backup.json              # replace the data with the file's
holdings import -telegram backup.json    # and restore the Telegram settings
```

**Stop the web app before `import`.** A running app keeps the data in memory and would overwrite the imported file the next time it saves. Importing through the Settings page doesn't have this problem.

With `go run`, use `go run . export backup.json`. The built binary is called `fundtrack` (or `fundtrack.exe`) unless you named it otherwise.

### Under Docker

Export from a running container:

```bash
docker exec holdings holdings export > holdings-backup.json
```

Import into a stopped container. This runs a one-off container against the same volume:

```bash
docker stop holdings
docker run --rm -i -v holdings-data:/data ghcr.io/this-will-be-your-username/holdings:latest import - < holdings-backup.json
docker start holdings
```

`-` reads the file from standard input. The Settings page is usually easier: it works while the container runs.

## How import behaves

- **It replaces, it doesn't merge.** All current data is swapped for the file's. To add transactions to existing data, use [Importing Transactions](Importing-Transactions.md) instead.
- **Your previous data is kept.** Before replacing anything, the current data file is copied to `fundtrack.json.YYYYMMDD-HHMMSS.bak` beside it. To undo an import, stop the app, copy the `.bak` file over `fundtrack.json` and start it again. Delete old `.bak` files when you no longer need them.
- **Nothing changes if the file is bad.** The file is checked first: unknown or damaged files, transactions with bad dates or no symbol, and duplicate transaction ids are refused with a message.
- **A data file works too.** You can import a plain `fundtrack.json` copied from another installation instead of an export. Either can be gzipped (`.json.gz`).
- **Newer exports are refused.** An export made by a newer version of Holdings, with a newer file format, asks you to upgrade first. Older exports always import.
- **Test mode:** with [Test Mode](Test-Mode.md) on, export and import work on the simulated dataset, and your real data is untouched.

## Moving to a new installation, step by step

1. On the old installation, open **Settings → Export and import** and press **Export everything** (with the Telegram option if you use the bot).
2. Set up the new installation ([Getting Started](Getting-Started.md) or [Running with Docker](Running-with-Docker.md)) and open it.
3. On its **Settings** page, import the file.
4. Press **Refresh prices**. Check the dashboard total matches the old installation.
5. If you restored the Telegram settings, turn off the bot on the old installation. A bot token can only be polled from one place at a time.
