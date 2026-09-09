# rw-chebur-monitor

[![CI](https://github.com/omssky/rw-chebur-monitor/actions/workflows/build.yml/badge.svg)](https://github.com/omssky/rw-chebur-monitor/actions/workflows/build.yml)

Automatic TSPU block monitoring for [Remnawave](https://docs.rw/). Checks node endpoints through [Cheburcheck](https://cheburcheck.ru/) and sends block and recovery alerts to your Telegram topic.

## Features

- **Automatic discovery** — keeps monitored endpoints in sync with Remnawave.
- **Dynamic checks** — runs Cheburcheck probes on a configurable schedule, every 30 minutes by default.
- **Confirmed alerts** — verifies blocks before notifying you and reports when access is restored.
- **Telegram topics** — delivers alerts to the group and topic you choose.
- **Persistent state** — preserves incidents and pending notifications across restarts.

## Quick start

You need Docker with Compose, a Remnawave API token, and a Telegram bot that can post to your topic.

```bash
git clone https://github.com/omssky/rw-chebur-monitor.git
cd rw-chebur-monitor
cp .env.example .env
```

Edit `.env` with your Remnawave URL and API token, Telegram bot token, chat ID, and topic ID. Available settings are listed in [.env.example](.env.example).

```bash
docker compose up -d
```

Compose uses the published image from `ghcr.io/omssky/rw-chebur-monitor`, available for `linux/amd64` and `linux/arm64`.

## Updating

```bash
git pull --ff-only
docker compose pull
docker compose up -d
```
