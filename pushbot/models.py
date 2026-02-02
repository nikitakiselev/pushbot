"""Модели базы данных для деплоев и сервисов."""
from sqlalchemy import Column, Integer, String, Text, DateTime, Boolean, ForeignKey
from sqlalchemy.orm import relationship
from datetime import datetime
from pushbot.database import Base


class Service(Base):
    """Модель сервиса для деплоя."""
    __tablename__ = "services"

    id = Column(Integer, primary_key=True, index=True)
    name = Column(String, unique=True, index=True, nullable=False)
    repository = Column(String, nullable=False)
    path = Column(String, nullable=False)
    branch = Column(String, nullable=False)
    deploy_command = Column(Text, nullable=False)
    created_at = Column(DateTime, default=datetime.utcnow)

    deployments = relationship("Deployment", back_populates="service")


class Deployment(Base):
    """Модель деплоя."""
    __tablename__ = "deployments"

    id = Column(Integer, primary_key=True, index=True)
    service_id = Column(Integer, ForeignKey("services.id"), nullable=False)
    status = Column(String, nullable=False)  # 'queued', 'running', 'success', 'failed'
    started_at = Column(DateTime, default=datetime.utcnow, nullable=False)
    finished_at = Column(DateTime, nullable=True)
    stdout = Column(Text, default="")
    stderr = Column(Text, default="")
    exit_code = Column(Integer, nullable=True)
    commit_sha = Column(String, nullable=True)
    commit_message = Column(String, nullable=True)
    branch = Column(String, nullable=True)

    service = relationship("Service", back_populates="deployments")
