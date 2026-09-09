# rw-chebur-monitor

Проверяет клиентские адреса VPN-нод из Remnawave через динамическую проверку Cheburcheck и отправляет алерты о ТСПУ-блокировках в Telegram-топик.

По умолчанию обходит адреса каждые 30 минут. Первое обнаружение перепроверяет через 3 минуты; два последовательных `tspu_block` от одного сканера подтверждают блокировку. Два результата `ok` подтверждают восстановление. Повторяющиеся блокировки не создают новые алерты, ошибки проверки не считаются восстановлением.

Выбирает включённые Hosts, связанные с активными нодами и inbounds; одинаковые адреса проверяет один раз. Cheburcheck работает с портом 443, остальные Hosts пропускаются. Это проверка сетевой блокировки со стороны сканеров Cheburcheck, а не полноценная проверка VPN-соединения.

## Запуск

Нужны Docker с Compose, API-токен Remnawave с доступом к Nodes/Hosts и Telegram-бот, которому разрешено писать в нужный топик.

```bash
git clone https://github.com/omssky/rw-chebur-monitor.git
cd rw-chebur-monitor
cp .env.example .env
chmod 600 .env
nano .env
```

В `.env` укажите адрес панели **без `/api`**, токены Remnawave и Telegram, ID группы и топика. `CHECK_INTERVAL=30m` и `CONFIRM_DELAY=3m` можно поменять; `DB_PATH` для готового Compose оставьте как есть.

```bash
docker compose pull
docker compose up -d
docker compose logs -f --tail=100 monitor
```

Образ `ghcr.io/omssky/rw-chebur-monitor:latest` собирается в GitHub Actions после тестов, доступен для `amd64` и `arm64`. Входящие порты не нужны.

Состояние блокировок и очередь сообщений хранятся в SQLite, в volume `monitor-data`, и переживают перезапуск. Не используйте `docker compose down -v`, если хотите сохранить эти данные. Неотправленные сообщения повторяются по очереди; при сбое сразу после отправки возможен дубль.

## Обновление

```bash
git pull --ff-only
docker compose pull
docker compose up -d
```
