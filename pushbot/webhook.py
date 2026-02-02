"""Обработка вебхуков от GitHub."""
import hmac
import hashlib
import os
from typing import Optional
from sqlalchemy.orm import Session
from pushbot.config import AppConfig, get_service_by_repository
from pushbot.models import Service
from pushbot.deployer import start_deployment


def verify_github_signature(payload_body: bytes, signature_header: str, secret: str) -> bool:
    """Проверить подпись GitHub webhook."""
    if not secret:
        return True  # Если секрет не задан, пропускаем проверку
    
    if not signature_header:
        return False
    
    # GitHub использует формат "sha256=<hash>"
    if not signature_header.startswith('sha256='):
        return False
    
    # Извлекаем хеш из заголовка
    expected_hash = signature_header[7:]
    
    # Вычисляем HMAC SHA256
    computed_hash = hmac.new(
        secret.encode('utf-8'),
        payload_body,
        hashlib.sha256
    ).hexdigest()
    
    # Сравниваем безопасным способом (constant-time comparison)
    return hmac.compare_digest(expected_hash, computed_hash)


async def handle_github_webhook(
    db: Session,
    config: AppConfig,
    payload: dict,
) -> Optional[dict]:
    """Обработать вебхук от GitHub и запустить деплой, если необходимо."""
    # Проверяем структуру payload
    if not isinstance(payload, dict):
        return {"error": "Payload должен быть словарем (JSON объектом)"}
    
    # Извлекаем информацию о репозитории и ветке
    repository = payload.get("repository")
    if not repository or not isinstance(repository, dict):
        return {"error": "Отсутствует или неверный формат поля 'repository' в payload"}
    
    repository_full_name = repository.get("full_name")
    if not repository_full_name:
        # Попробуем альтернативный формат
        owner = repository.get("owner", {})
        if isinstance(owner, dict):
            owner_name = owner.get("login") or owner.get("name")
            repo_name = repository.get("name")
            if owner_name and repo_name:
                repository_full_name = f"{owner_name}/{repo_name}"
    
    ref = payload.get("ref", "")
    if not ref:
        return {"error": "Отсутствует поле 'ref' в payload"}
    
    branch = ref.replace("refs/heads/", "") if ref.startswith("refs/heads/") else None

    if not repository_full_name or not branch:
        return {
            "error": "Не удалось определить репозиторий или ветку",
            "details": {
                "repository_full_name": repository_full_name,
                "ref": ref,
                "branch": branch
            }
        }

    # Находим сервис в конфигурации
    service_config = get_service_by_repository(config, repository_full_name, branch)
    if not service_config:
        return {"error": f"Сервис для репозитория {repository_full_name} и ветки {branch} не найден"}

    # Получаем или создаем сервис в базе данных
    service = db.query(Service).filter(Service.name == service_config.name).first()
    if not service:
        service = Service(
            name=service_config.name,
            repository=service_config.repository,
            path=service_config.path,
            branch=service_config.branch,
            deploy_command=service_config.deploy_command,
        )
        db.add(service)
        db.commit()
        db.refresh(service)
    else:
        # Обновляем конфигурацию сервиса, если она изменилась
        service.repository = service_config.repository
        service.path = service_config.path
        service.branch = service_config.branch
        service.deploy_command = service_config.deploy_command
        db.commit()

    # Извлекаем информацию о коммите
    commits = payload.get("commits", [])
    commit_sha = None
    commit_message = None
    if commits:
        latest_commit = commits[-1]
        commit_sha = latest_commit.get("id")
        commit_message = latest_commit.get("message", "")

    # Запускаем деплой
    deployment_id = await start_deployment(
        db=db,
        service=service,
        command=service_config.deploy_command,
        commit_sha=commit_sha,
        commit_message=commit_message,
        branch=branch,
        triggered_by="webhook",
    )

    return {
        "message": "Деплой запущен",
        "deployment_id": deployment_id,
        "service": service_config.name,
    }
