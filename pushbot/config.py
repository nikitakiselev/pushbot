"""Загрузка и валидация конфигурации из YAML."""
import yaml
from pathlib import Path
from typing import List, Dict, Any, Optional
from pydantic import BaseModel, Field


class ServiceConfig(BaseModel):
    """Конфигурация одного сервиса."""
    name: str
    repository: str
    path: str
    branch: str
    deploy_command: str


class AppConfig(BaseModel):
    """Конфигурация приложения."""
    services: List[ServiceConfig] = Field(default_factory=list)


def load_config(config_path: str = "config.yaml") -> AppConfig:
    """Загрузить конфигурацию из YAML файла."""
    config_file = Path(config_path)
    if not config_file.exists():
        raise FileNotFoundError(f"Конфигурационный файл {config_path} не найден")

    with open(config_file, "r", encoding="utf-8") as f:
        config_data = yaml.safe_load(f)

    return AppConfig(**config_data)


def get_service_by_repository(config: AppConfig, repository: str, branch: str) -> Optional[ServiceConfig]:
    """Найти сервис по репозиторию и ветке."""
    for service in config.services:
        if service.repository == repository and service.branch == branch:
            return service
    return None
