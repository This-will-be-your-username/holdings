# Running with Docker

Holdings ships with a `Dockerfile`, so you can run it without installing Go. The image is a small Alpine container holding one static binary. Your data lives in a volume outside the container, so it survives rebuilds and upgrades.

## Requirements

- [Docker](https://docs.docker.com/get-docker/) (Docker Desktop on Windows or macOS, Docker Engine on Linux).
- A clone of the repository. The image is built from source; there is no published image.

## Build

From the repository root:

```bash
docker build -t holdings .
```

## Run

```bash
docker run -d --name holdings \
  -p 127.0.0.1:8080:8080 \
  -v holdings-data:/data \
  holdings
```

Open <http://127.0.0.1:8080>.

What the options do:

| Option | Meaning |
|---|---|
| `-p 127.0.0.1:8080:8080` | Publishes the app on your own machine only. Use `-p 127.0.0.1:9000:8080` for a different host port. |
| `-v holdings-data:/data` | A named volume that holds your data file and the files beside it. |
| `-d --name holdings` | Runs in the background with a name you can refer to later. |

> **Security note.** The app has no login, and inside the container it listens on `0.0.0.0` (so Docker can reach it). Always publish the port with `127.0.0.1:` in front, as above. Writing just `-p 8080:8080` exposes it to your whole network, and anyone who can reach it can read and change your data. See [Configuration and Data](Configuration-and-Data.md).

## Docker Compose

If you prefer Compose, use the `compose.yaml` in the repository root (or save this as `compose.yaml` next to the `Dockerfile`):

```yaml
services:
  holdings:
    build: .
    ports: ["${HOLDINGS_BIND:-127.0.0.1}:${HOLDINGS_PORT:-8080}:8080"]
    volumes: ["${HOLDINGS_DATA:-holdings-data}:/data"]
    restart: ${HOLDINGS_RESTART:-unless-stopped}

volumes:
  holdings-data:
```

```bash
docker compose up -d --build
```

Every value has a default, so it works as is. To change one, set the variable in your shell or in a `.env` file beside `compose.yaml`:

| Variable | Default | Meaning |
|---|---|---|
| `HOLDINGS_BIND` | `127.0.0.1` | Host address to publish on. Leave it on the loopback address (see the security note above). |
| `HOLDINGS_PORT` | `8080` | Host port. |
| `HOLDINGS_DATA` | `holdings-data` | Named volume, or a folder path such as `./data`, for your data. |
| `HOLDINGS_RESTART` | `unless-stopped` | Restart policy. |

## Settings

The image sets `ADDR=0.0.0.0:8080` and `DATA_FILE=/data/fundtrack.json`. Other [environment variables](Getting-Started.md) can be passed with `-e`:

```bash
docker run -d --name holdings -p 127.0.0.1:8080:8080 -v holdings-data:/data -e TEST_MODE=1 holdings
```

Leave `ADDR` and `DATA_FILE` alone unless you also change the port and volume to match. Telegram and most other settings are configured in the app itself, on the **Settings** page ([Telegram Bot](Telegram-Bot.md)), and are stored in the data volume.

## Your data

Inside the container everything is under `/data` (`fundtrack.json`, `fundtrack.json.quotes`, `fundtrack-test.json`, `mode`; see [Getting Started](Getting-Started.md)). The container runs as an unprivileged user (uid 10001).

**Back up** by copying the data file out of the running container:

```bash
docker cp holdings:/data/fundtrack.json ./fundtrack-backup.json
```

**Restore** by copying it back in and restarting:

```bash
docker cp ./fundtrack-backup.json holdings:/data/fundtrack.json
docker restart holdings
```

**Use a folder instead of a named volume** (for example to keep the data beside your other files) by swapping the `-v` option for `-v "$(pwd)/data:/data"` (PowerShell: `-v "${PWD}/data:/data"`). On Linux the folder must be writable by uid 10001, for example `sudo chown 10001 data`.

If you already have a `data/fundtrack.json` from running the app directly, you can use it with the bind mount above.

## Upgrading

Pull the new code, rebuild and recreate the container. The volume is kept, so your data stays.

```bash
git pull
docker build -t holdings .
docker rm -f holdings
docker run -d --name holdings -p 127.0.0.1:8080:8080 -v holdings-data:/data holdings
```

(With Compose, `git pull` then `docker compose up -d --build`.)

## Troubleshooting

| Symptom | Fix |
|---|---|
| `docker logs holdings` shows a permission error on `/data` | A bind-mounted folder isn't writable by uid 10001. Change its ownership, or use a named volume. |
| The page doesn't load | Check `docker ps` shows the container running and that the host port isn't already in use. Pick another with `-p 127.0.0.1:9000:8080`. |
| Prices are missing | The container needs outbound internet access for live prices. See [Prices and Exchange Rates](Prices-and-Exchange-Rates.md). |
| Times look wrong | The image includes timezone data. Add `-e TZ=Australia/Sydney` if you want logs in local time. |
| `docker rm -f` deleted my data | It didn't, as long as you used a volume or bind mount. `docker volume rm holdings-data` is what deletes it. |
