# Развёртывание AIHub на VPS

```
Интернет / туннель OpenFlux
        │ 443
   ┌────▼─────┐   docker compose (/opt/aihub)
   │  Caddy   │── TLS Let's Encrypt
   └────┬─────┘
        │ :8080 (внутренняя сеть compose)
 ┌──────▼────────┐   unix-сокет /run/aihub-nodectl/nodectl.sock (группа aihub)
 │ aihub-backend │──────────────────────────────┐
 └───────────────┘                              │
 хост (systemd):                                ▼
   aihub-nodectl (root) → /usr/local/lib/aihub/node-install.sh → openflux-node@<канал>
```

## Что нужно

- VPS с Debian 12 / Ubuntu 22.04+ (amd64 или arm64), systemd, root по SSH-ключу.
- Домен, указывающий на VPS, или ничего: тогда используется `<ip>.sslip.io`.
- Открытые 80 и 443 (80 нужен Let's Encrypt).

## Установка

```sh
git clone --depth 1 https://github.com/ItzChelove4ik/AIHub /tmp/aihub
sudo sh /tmp/aihub/deploy/install.sh --admin-email you@example.com --ip 203.0.113.7
# или: --domain aihub.example.com
```

Скрипт можно запускать повторно — так же делается обновление (`--version backend-vX.Y.Z`).
`.env` с секретами он не перезаписывает.

| Параметр | Значение |
|---|---|
| `--admin-email` | Единственный аккаунт, который регистрируется без приглашения и становится админом |
| `--domain` / `--ip` | Домен или IPv4 (тогда домен `<ip>.sslip.io`) |
| `--version` | Тег релиза `backend-v*` (по умолчанию последний) |
| `--binaries DIR` | Взять собранные бинарники из папки (с `SHA256SUMS`) вместо релиза |
| `--registration` | `invite` (по умолчанию) или `open` |
| `--enable-ufw` | Включить ufw, если выключен (порт SSH остаётся открытым, `--ssh-port`) |

## Что где лежит

| Путь | Что |
|---|---|
| `/opt/aihub/.env` | Настройки и **`AIHUB_MASTER_KEY`** (0600). Сохраните ключ отдельно: без него ключи провайдеров и бэкапы не расшифровать |
| `/opt/aihub/docker-compose.yml`, `Caddyfile` | Сервисы |
| `/usr/local/bin/aihub-nodectl`, `aihub-nodectl.service` | Хелпер, root, разрешает только `apply/remove/list/status` |
| `/usr/local/lib/aihub/node-install.sh` | Скрипт OpenFlux, закреплён на коммите с проверкой SHA-256 (как в `provision/pin.go`) |
| `/var/backups/aihub` | Ежедневные копии БД (`aihub-backup.timer`), 14 дней |

Почему хелпер не вызывает `/opt/openflux-node/node-install.sh`: эту папку скрипт удаляет
целиком при удалении последнего канала, а его автообновление переписывает файл там.

## Безопасность

- Бэкенду не дают Docker-сокет и sudo: только сокет хелпера, который принимает четыре
  операции и проверяет каждую строку конфига; конфиг с ключом канала уходит во временный
  файл 0600, не в аргументы.
- Контейнер бэкенда: read-only, без capabilities, no-new-privileges, пользователь nonroot.
- Логи Caddy без `code`/`state` OAuth; заголовки авторизации Caddy скрывает сам.

## Полезные команды

```sh
cd /opt/aihub
docker compose ps
docker compose logs -f aihub-backend
journalctl -u aihub-nodectl -f
aihub-nodectl list                 # каналы OpenFlux и их состояние
systemctl start aihub-backup       # бэкап сейчас
```
