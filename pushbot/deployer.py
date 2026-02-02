"""Модуль для выполнения деплоев в изолированных процессах."""
import subprocess
import asyncio
from typing import Callable, Optional
from datetime import datetime
from sqlalchemy.orm import Session
from pushbot.models import Deployment, Service


class DeploymentRunner:
    """Класс для запуска и мониторинга деплоев."""

    def __init__(self, db: Session, deployment_id: int):
        self.db = db
        self.deployment_id = deployment_id
        self.process: Optional[subprocess.Popen] = None
        self.stdout_buffer = []
        self.stderr_buffer = []

    async def run(
        self,
        command: str,
        cwd: str,
        on_stdout: Optional[Callable[[str], None]] = None,
        on_stderr: Optional[Callable[[str], None]] = None,
    ) -> int:
        """Запустить команду деплоя и собрать логи в реальном времени."""
        # Статус уже должен быть 'running' при вызове

        # Запускаем процесс
        self.process = subprocess.Popen(
            command,
            shell=True,
            cwd=cwd,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            bufsize=1,
            universal_newlines=True,
        )

        # Создаем задачи для чтения stdout и stderr
        stdout_task = asyncio.create_task(
            self._read_stream(self.process.stdout, self.stdout_buffer, on_stdout)
        )
        stderr_task = asyncio.create_task(
            self._read_stream(self.process.stderr, self.stderr_buffer, on_stderr)
        )

        # Ждем завершения процесса
        exit_code = await asyncio.to_thread(self.process.wait)

        # Ждем завершения чтения потоков
        await stdout_task
        await stderr_task

        # Сохраняем результаты в базу данных
        deployment = self.db.query(Deployment).filter(Deployment.id == self.deployment_id).first()
        if deployment:
            deployment.status = "success" if exit_code == 0 else "failed"
            deployment.finished_at = datetime.utcnow()
            deployment.exit_code = exit_code
            deployment.stdout = "".join(self.stdout_buffer)
            deployment.stderr = "".join(self.stderr_buffer)
            self.db.commit()

        return exit_code

    async def _read_stream(
        self,
        stream,
        buffer: list,
        callback: Optional[Callable[[str], None]] = None,
    ):
        """Читать поток построчно и сохранять в буфер."""
        if stream is None:
            return

        loop = asyncio.get_event_loop()
        while True:
            try:
                line = await loop.run_in_executor(None, stream.readline)
                if not line:
                    break
                buffer.append(line)
                if callback:
                    callback(line)
            except Exception:
                break

    def stop(self):
        """Остановить процесс деплоя."""
        if self.process and self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.process.kill()


# Глобальный словарь для отслеживания активных деплоев
active_deployments: dict[int, DeploymentRunner] = {}


async def start_deployment(
    db: Session,
    service: Service,
    command: str,
    commit_sha: Optional[str] = None,
    commit_message: Optional[str] = None,
    branch: Optional[str] = None,
) -> int:
    """Запустить новый деплой или поставить в очередь."""
    from pushbot.database import SessionLocal
    
    # Проверяем, есть ли уже активный деплой для этого сервиса
    active_deployment = db.query(Deployment).filter(
        Deployment.service_id == service.id,
        Deployment.status == "running"
    ).first()
    
    # Создаем запись о деплое в базе данных
    deployment = Deployment(
        service_id=service.id,
        status="queued" if active_deployment else "running",
        started_at=datetime.utcnow(),
        commit_sha=commit_sha,
        commit_message=commit_message,
        branch=branch or service.branch,
    )
    db.add(deployment)
    db.commit()
    db.refresh(deployment)
    deployment_id = deployment.id

    # Если есть активный деплой, просто возвращаем ID (деплой в очереди)
    if active_deployment:
        return deployment_id

    # Запускаем деплой сразу
    await _run_deployment_async(deployment_id, service, command)

    return deployment_id


async def _run_deployment_async(deployment_id: int, service: Service, command: str):
    """Запустить деплой асинхронно."""
    from pushbot.database import SessionLocal
    
    # Создаем новую сессию БД для фоновой задачи
    async def run_deployment():
        db_session = SessionLocal()
        try:
            runner = DeploymentRunner(db_session, deployment_id)
            active_deployments[deployment_id] = runner
            try:
                await runner.run(command, service.path)
            finally:
                # Удаляем из активных деплоев после завершения
                active_deployments.pop(deployment_id, None)
                # Проверяем очередь и запускаем следующий деплой
                # Используем новую сессию для обработки очереди
                queue_db = SessionLocal()
                try:
                    await _process_queue_for_service(queue_db, service.id)
                finally:
                    queue_db.close()
        finally:
            db_session.close()

    # Создаем задачу в текущем event loop
    loop = asyncio.get_event_loop()
    loop.create_task(run_deployment())


async def _process_queue_for_service(db: Session, service_id: int):
    """Обработать очередь деплоев для сервиса - запустить следующий в очереди."""
    from pushbot.models import Service
    
    # Находим следующий деплой в очереди для этого сервиса
    queued_deployment = db.query(Deployment).filter(
        Deployment.service_id == service_id,
        Deployment.status == "queued"
    ).order_by(Deployment.started_at.asc()).first()
    
    if not queued_deployment:
        return
    
    # Получаем сервис
    service = db.query(Service).filter(Service.id == service_id).first()
    if not service:
        return
    
    # Обновляем статус на running
    queued_deployment.status = "running"
    db.commit()
    db.refresh(queued_deployment)
    
    # Запускаем деплой (используем команду из сервиса, так как в queued деплое команда может быть не сохранена)
    await _run_deployment_async(queued_deployment.id, service, service.deploy_command)


def get_active_deployment(deployment_id: int) -> Optional[DeploymentRunner]:
    """Получить активный деплой по ID."""
    return active_deployments.get(deployment_id)


def get_all_active_deployments() -> dict[int, DeploymentRunner]:
    """Получить все активные деплои."""
    return active_deployments.copy()
