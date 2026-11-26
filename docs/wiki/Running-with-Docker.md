# Running with Docker

Holdings is published as a container image on GitHub (`ghcr.io/this-will-be-your-username/holdings`), so you can run it without installing Go. The image is a small Alpine container holding one static binary. Your data lives in a volume outside the container, so it survives upgrades.

## Requirements

- [Docker](https://docs.docker.com/get-docker/) (Docker Desktop on Windows or macOS, Docker Engine on Linux).
- Nothing else for Compose: it pulls the image from GitHub. A clone of the repository is only needed to build the image yourself (see Build).

## Build (optional)

The commands on this page use the published image. To build it yourself from source instead, from the repository root:

```bash
docker build -t holdings .
```

## Run

```bash
docker run -d --name holdings \
  -p 127.0.0.1:8080:8080 \
  -v holdings-data:/data \
  ghcr.io/this-will-be-your-username/holdings:latest
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

The easiest way. Save this as `compose.yaml` (or use the one in the repository root). It pulls the published image, so no clone or build is needed:

```yaml
---
services:
  holdings:
    image: ghcr.io/this-will-be-your-username/holdings:latest
    container_name: holdings
    environment:
      - TZ=Etc/UTC
    volumes:
      - holdings-data:/data
    ports:
      - 127.0.0.1:8080:8080
    restart: unless-stopped

volumes:
  holdings-data:
```

```bash
docker compose up -d
```

The layout follows the one [LinuxServer.io](https://www.linuxserver.io/) uses for its images. It works as is; to change something, edit the file:

| Setting | Meaning |
|---|---|
| `image` | Image to run. Pin a version tag such as `:1.2.3` if you like, or use a locally built image such as `holdings`. |
| `container_name` | The name used by `docker logs`, `docker cp` and the other commands on this page. |
| `TZ` | Your [timezone](https://en.wikipedia.org/wiki/List_of_tz_database_time_zones), for example `Australia/Sydney`. Affects log times. |
| `holdings-data:/data` | Where your data lives. Replace `holdings-data` with a folder path such as `./data` to keep it beside your other files (see Your data below), and then remove the `volumes:` block at the bottom. |
| `127.0.0.1:8080:8080` | Host address, host port, container port. Change the middle number for a different host port, and keep `127.0.0.1:` in front (see the security note above). |
| `restart` | Restart policy. |

## Settings

The image sets `ADDR=0.0.0.0:8080` and `DATA_FILE=/data/fundtrack.json`. Other [environment variables](Getting-Started.md) can be passed with `-e`:

```bash
docker run -d --name holdings -p 127.0.0.1:8080:8080 -v holdings-data:/data -e TEST_MODE=1 ghcr.io/this-will-be-your-username/holdings:latest
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

**Move to another installation** with **Export everything** on the Settings page, or `docker exec holdings holdings export > holdings-backup.json`. See [Export and Import](Export-and-Import.md).

**Use a folder instead of a named volume** (for example to keep the data beside your other files) by swapping the `-v` option for `-v "$(pwd)/data:/data"` (PowerShell: `-v "${PWD}/data:/data"`). On Linux the folder must be writable by uid 10001, for example `sudo chown 10001 data`.

If you already have a `data/fundtrack.json` from running the app directly, you can use it with the bind mount above.

## Upgrading

Pull the new image and recreate the container. The volume is kept, so your data stays.

```bash
docker compose pull
docker compose up -d
```

If you built the image yourself, run `git pull`, then `docker build -t holdings .`, then `docker rm -f holdings` and the `docker run` command above with `holdings` as the image name.

## Troubleshooting

| Symptom | Fix |
|---|---|
| `docker logs holdings` shows a permission error on `/data` | A bind-mounted folder isn't writable by uid 10001. Change its ownership, or use a named volume. |
| The page doesn't load | Check `docker ps` shows the container running and that the host port isn't already in use. Pick another with `-p 127.0.0.1:9000:8080`. |
| Prices are missing | The container needs outbound internet access for live prices. See [Prices and Exchange Rates](Prices-and-Exchange-Rates.md). |
| Times look wrong | The image includes timezone data. Add `-e TZ=Australia/Sydney` if you want logs in local time. |
| `docker rm -f` deleted my data | It didn't, as long as you used a volume or bind mount. `docker volume rm holdings-data` is what deletes it. |
