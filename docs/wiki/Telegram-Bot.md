# Telegram Bot

Holdings can talk to you through a Telegram bot, so you can check your portfolio from your phone without opening the app. It can pull fresh prices, send a snapshot of the latest prices and draw small graphs of the last week.

## Setting it up

Everything is configured on the **Settings** page.

1. In Telegram, open **@BotFather**, send `/newbot` and follow the prompts. It gives you a token.
2. On **Settings**, paste the token, tick **Turn the bot on** and press **Save**. The token is checked with Telegram before it is saved.
3. Open your new bot in Telegram and send `/start`. The bot replies that it is private, and the chat appears on the Settings page.
4. Press **Approve this chat**. From then on the bot answers that chat only.

You can also type a chat id into **Approved chat id** yourself. **Send a test message** confirms the link, and **Remove Telegram settings** deletes the token and approval from your computer.

## Commands

| Command | What it does |
|---|---|
| `/refresh` | Pulls fresh prices (the same as the dashboard's refresh), then sends a snapshot. |
| `/prices` | Sends a snapshot: each holding's latest price, its move since the previous saved price, and the portfolio value. |
| `/graph` | Sends a small graph of the last seven days for each holding (up to 10 at once). |
| `/graph VGS` | The graph for one holding. |
| `/help` | Lists the commands. |

Every snapshot ends with buttons offering the graphs: **All holdings**, or one button per holding. Each graph is a small PNG (green when the week is up, red when down) with the latest price, the change over the week, and the low and high in the caption. If a holding has fewer than two saved prices in the window, its last few saved prices are used, so a long weekend still draws something. Private companies have no price history and are left out.

## Privacy and security

- The app connects **out** to Telegram (long polling). No port is opened and the app can stay bound to the loopback address.
- The bot answers only the one approved chat. Anyone else who messages it is refused and receives no data; their name and chat id appear on the Settings page so you can approve them if it was you.
- Messages contain prices, symbols and your portfolio total, and travel through Telegram's servers. Transactions, units and cost basis are never sent.
- The token is stored in `telegram.json` beside the data file (owner-only permissions), not in the data file, so a backup or shared copy of your data does not contain it. Treat the file like a password: anyone with the token can control the bot. If it leaks, revoke it with BotFather's `/revoke`.
- In [Test Mode](Test-Mode.md) the bot answers with the simulated data and labels it as such; no prices are fetched.
