# PushBot - Домашний деплой-сервис

PushBot - это автоматизированный деплой-сервис на базе FastAPI, который слушает вебхуки от GitHub и автоматически деплоит ваши проекты.

## Возможности

- Автоматический деплой при получении вебхуков от GitHub
- Поддержка нескольких параллельных деплоев
- Мониторинг деплоев в реальном времени через веб-интерфейс
- История всех деплоев с логами
- Конфигурация через YAML файл

## Установка

### Быстрая установка (рекомендуется)

Используйте скрипт автоматической настройки:
```bash
./setup.sh
```

Скрипт автоматически:
- Создаст виртуальное окружение `venv`
- Установит все зависимости
- Настроит окружение для разработки

### Ручная установка

1. Создайте виртуальное окружение:
```bash
python3 -m venv venv
```

2. Активируйте виртуальное окружение:
```bash
# На macOS/Linux:
source venv/bin/activate

# На Windows:
venv\Scripts\activate
```

3. Установите зависимости:
```bash
pip install --upgrade pip
pip install -r requirements.txt
```

4. Настройте конфигурацию в файле `config.yaml`

5. Запустите сервис:
```bash
# Вариант 1: Используя скрипт run.py (рекомендуется)
python run.py

# Вариант 2: Напрямую через uvicorn
uvicorn pushbot.main:app --host 0.0.0.0 --port 8009
```

**Важно**: Скрипт `run.py` автоматически проверяет наличие активированного venv и предупреждает, если оно не активировано.

## Настройка GitHub Webhook

1. Перейдите в настройки вашего репозитория на GitHub
2. Выберите Settings → Webhooks → Add webhook
3. Укажите URL: `http://your-server:8009/webhook`
4. Content type: `application/json`
5. Выберите события: `Just the push event`
6. (Опционально) Укажите Secret для дополнительной безопасности

### Безопасность Webhook

Для защиты от несанкционированных запросов можно использовать секрет webhook:

1. Установите переменную окружения `GITHUB_WEBHOOK_SECRET`:
```bash
export GITHUB_WEBHOOK_SECRET="your-secret-key-here"
```

2. Укажите тот же секрет в настройках webhook на GitHub

Если переменная `GITHUB_WEBHOOK_SECRET` не задана, проверка подписи не выполняется (для обратной совместимости).

## Конфигурация

Файл `config.yaml` содержит список сервисов для деплоя:

```yaml
services:
  - name: my-app
    repository: "owner/repo-name"
    path: "/home/user/projects/my-app"
    branch: "main"
    deploy_command: "cd /home/user/projects/my-app && git pull && ./deploy.sh"
```

## Веб-интерфейс

Веб-интерфейс доступен по адресу `http://your-server:8009/` и предоставляет:
- Список текущих выполняющихся деплоев с логами в реальном времени (Server-Sent Events)
- Историю последних деплоев с полными логами
- Список настроенных сервисов

## API Endpoints

- `GET /` - Веб-интерфейс
- `POST /webhook` - Эндпоинт для вебхуков от GitHub
- `GET /api/deployments/active` - Список активных деплоев
- `GET /api/deployments/{deployment_id}` - Информация о конкретном деплое
- `GET /api/deployments/{deployment_id}/logs` - Логи деплоя в реальном времени (SSE)
- `GET /api/deployments` - Список всех деплоев (с фильтрацией по статусу)

## Структура проекта

```
pushbot/
├── pushbot/
│   ├── __init__.py
│   ├── main.py          # Главный файл FastAPI приложения
│   ├── config.py        # Загрузка конфигурации из YAML
│   ├── database.py      # Инициализация базы данных
│   ├── models.py        # SQLAlchemy модели
│   ├── deployer.py      # Модуль выполнения деплоев
│   ├── webhook.py       # Обработка вебхуков GitHub
│   └── templates/
│       └── index.html   # Веб-интерфейс
├── config.yaml          # Конфигурация сервисов
├── requirements.txt     # Зависимости Python
├── run.py              # Скрипт запуска
└── README.md           # Документация
```

## Особенности

- **Параллельные деплои**: Поддержка нескольких одновременных деплоев
- **Изолированные процессы**: Каждый деплой запускается в отдельном процессе
- **Сбор логов в реальном времени**: stdout и stderr собираются построчно
- **SQLite база данных**: История деплоев сохраняется в локальной БД
- **Server-Sent Events**: Логи передаются в браузер в реальном времени


## Run as daemon

echo "GITHUB_WEBHOOK_SECRET=YOUR_GITHUB_WEBHOOK_TOKEN" > /opt/pushbot/.env

*~/.config/systemd/user/pushbot.service*
```
[Unit]
Description=PushBot
After=network.target

[Service]
ExecStart=/opt/pushbot/pushbot serve
WorkingDirectory=/opt/pushbot
EnvironmentFile=/opt/pushbot/.env
Restart=always
RestartSec=2

[Install]
WantedBy=default.target
```

```
systemctl --user daemon-reload
systemctl --user enable pushbot
systemctl --user start pushbot

systemctl --user status pushbot
journalctl --user -u pushbot -f

systemctl --user restart pushbot
```