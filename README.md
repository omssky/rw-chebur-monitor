# rw-chebur-monitor

[![CI](https://github.com/omssky/rw-chebur-monitor/actions/workflows/build.yml/badge.svg)](https://github.com/omssky/rw-chebur-monitor/actions/workflows/build.yml)

Небольшой сервис на Go для мониторинга ТСПУ-блокировок VPN-нод из [Remnawave](https://docs.rw/) через [Cheburcheck](https://cheburcheck.ru/).

## Возможности

- Автоматическое обновление списка адресов из Remnawave.
- Динамические проверки на ТСПУ-блокировки — по умолчанию раз в 30 минут.
- Уведомления о блокировке и восстановлении в выбранный топик Telegram.
- Сохранение состояния в SQLite между перезапусками.

## Быстрый запуск

Нужны Docker с Compose и Telegram-бот с доступом к нужному топику.

```bash
git clone https://github.com/omssky/rw-chebur-monitor.git
cd rw-chebur-monitor
cp .env.example .env
```

Заполните в `.env` адрес панели, API-токен Remnawave, токен Telegram-бота, ID группы и ID топика.

```bash
docker compose up -d
```

## Обновление

```bash
git pull --ff-only
docker compose pull
docker compose up -d
```
