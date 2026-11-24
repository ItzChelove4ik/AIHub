# AIHub

Единое окно к разным ИИ (OpenAI, Anthropic Claude, Google Gemini, OpenRouter и любые
OpenAI-совместимые API) для iPhone (iOS 26, установка через Sideloadly).

Вся логика, ключи провайдеров и запросы к ИИ живут на собственном сервере (VPS).
Приложение достаёт до сервера даже при «белых списках» мобильного интернета через
встроенное ядро [OpenFlux](https://github.com/p1neappleXpress/OpenFlux)
(туннель через документы / cups.online / прямой канал, локальный SOCKS5 внутри приложения).

```
iPhone: AIHub
  ├─ liboflux (ядро OpenFlux) → SOCKS5 127.0.0.1:1080 внутри приложения
  └─ URLSession / WKWebView ─► сначала напрямую https://<домен>; не вышло → через SOCKS5
VPS:
  openflux-node@<channel> (по одному на устройство) ─► Caddy (TLS) ─► aihub-backend ─► провайдеры ИИ
```

## Структура

| Папка | Что внутри |
|---|---|
| `backend/` | Go-сервер: аккаунты, ключи провайдеров, единый API чата, OpenAI-совместимый API, управление каналами OpenFlux |
| `ios/` | SwiftUI-приложение (XcodeGen) |
| `deploy/` | docker-compose, Caddyfile, скрипты установки на VPS |
| `third_party/openflux` | git submodule OpenFlux, закреплён на коммите |
| `docs/` | архитектура, OpenAPI (`docs/api.yaml`), инструкция по Sideloadly |
| `.github/workflows/` | CI бэкенда и сборка IPA |

Клонирование вместе с submodule:

```sh
git clone --recurse-submodules https://github.com/ItzChelove4ik/AIHub
```

## Безопасность

Репозиторий публичный. В нём **никогда** не бывает секретов: ссылок на документы-носители,
ключей каналов, cookies, адресов VPS, API-ключей. Всё это хранится только в `.env` на сервере;
в репозитории — заглушки (`.env.example`).

## Условия провайдеров

AIHub работает только через официальные API (ваши API-ключи) и официальный OAuth OpenRouter.
Вход через потребительские подписки (ChatGPT Plus, Claude Pro и т.п.) не поддерживается — это
запрещено условиями провайдеров. Часть провайдеров официально не обслуживает некоторые регионы;
запросы уходят с вашего сервера, и соблюдение условий провайдеров — ответственность владельца сервера.

## Лицензия

GPL-3.0 (см. `LICENSE`). Приложение встраивает ядро OpenFlux, которое распространяется под GPL-3.0.
