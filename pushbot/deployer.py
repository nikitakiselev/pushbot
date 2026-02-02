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
        # Единый буфер для упорядоченных логов: (timestamp, line, stream_type)
        self.ordered_logs = []
        self.deployment_start_time = None
        self.triggered_by = None

    def _format_log_line(self, line: str, stream_type: str = "stdout") -> str:
        """Форматировать строку лога с временной меткой."""
        if not line:
            return line
        timestamp = datetime.now().strftime("%Y-%m-%d %H:%M:%S")
        return f"[{timestamp}] {line}"

    def _add_log(self, line: str, stream_type: str = "stdout", callback: Optional[Callable[[str], None]] = None):
        """Добавить строку в лог с временной меткой."""
        if not line or (isinstance(line, str) and not line.strip() and line != '\n'):
            return
        timestamp = datetime.now()
        formatted_line = self._format_log_line(line, stream_type)
        
        # Добавляем в соответствующий буфер для обратной совместимости
        if stream_type == "stdout":
            self.stdout_buffer.append(formatted_line)
        else:
            self.stderr_buffer.append(formatted_line)
        
        # Добавляем в упорядоченный буфер
        self.ordered_logs.append((timestamp, formatted_line, stream_type))
        
        if callback:
            callback(formatted_line)

    async def run(
        self,
        command: str,
        cwd: str,
        triggered_by: Optional[str] = None,
        on_stdout: Optional[Callable[[str], None]] = None,
        on_stderr: Optional[Callable[[str], None]] = None,
    ) -> int:
        """Запустить команду деплоя и собрать логи в реальном времени."""
        # Статус уже должен быть 'running' при вызове
        self.deployment_start_time = datetime.now()
        self.triggered_by = triggered_by
        
        # Получаем информацию о деплое для логирования
        deployment = self.db.query(Deployment).filter(Deployment.id == self.deployment_id).first()
        service_name = "Unknown"
        if deployment:
            service = self.db.query(Service).filter(Service.id == deployment.service_id).first()
            if service:
                service_name = service.name
        
        # Добавляем событие начала деплоя
        trigger_info = f"triggered by {triggered_by}" if triggered_by else "triggered manually"
        start_message = f"[DEPLOY START] Service: {service_name}, Command: {command}, {trigger_info}\n"
        self._add_log(start_message, "stdout", on_stdout)

        # Запускаем процесс с отключенной буферизацией
        # Используем env для установки PYTHONUNBUFFERED и других переменных для разбуферизации
        import os
        env = os.environ.copy()
        env['PYTHONUNBUFFERED'] = '1'
        
        self.process = subprocess.Popen(
            command,
            shell=True,
            cwd=cwd,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            bufsize=0,  # Небуферизованный режим
            universal_newlines=True,
            env=env,
        )

        # Создаем задачи для чтения stdout и stderr
        stdout_task = asyncio.create_task(
            self._read_stream(self.process.stdout, "stdout", on_stdout)
        )
        stderr_task = asyncio.create_task(
            self._read_stream(self.process.stderr, "stderr", on_stderr)
        )

        # Ждем завершения процесса
        exit_code = await asyncio.to_thread(self.process.wait)

        # Ждем завершения чтения потоков
        await stdout_task
        await stderr_task

        # Добавляем событие завершения деплоя
        deployment_end_time = datetime.now()
        duration = (deployment_end_time - self.deployment_start_time).total_seconds()
        status_text = "SUCCESS" if exit_code == 0 else "FAILED"
        end_message = f"[DEPLOY END] Status: {status_text}, Exit Code: {exit_code}, Duration: {duration:.2f}s\n"
        self._add_log(end_message, "stdout", on_stdout)

        # Сохраняем результаты в базу данных
        deployment = self.db.query(Deployment).filter(Deployment.id == self.deployment_id).first()
        if deployment:
            deployment.status = "success" if exit_code == 0 else "failed"
            deployment.finished_at = datetime.utcnow()
            deployment.exit_code = exit_code
            
            # Сортируем логи по времени и разделяем на stdout и stderr
            if self.ordered_logs:
                sorted_logs = sorted(self.ordered_logs, key=lambda x: x[0])
                stdout_lines = []
                stderr_lines = []
                for _, line, stream_type in sorted_logs:
                    if stream_type == "stdout":
                        stdout_lines.append(line)
                    else:
                        stderr_lines.append(line)
                
                deployment.stdout = "".join(stdout_lines)
                deployment.stderr = "".join(stderr_lines)
            else:
                # Fallback: используем старые буферы, если ordered_logs пуст
                deployment.stdout = "".join(self.stdout_buffer)
                deployment.stderr = "".join(self.stderr_buffer)
            self.db.commit()

        return exit_code

    async def _read_stream(
        self,
        stream,
        stream_type: str,
        callback: Optional[Callable[[str], None]] = None,
    ):
        """Читать поток построчно и сохранять в буфер."""
        if stream is None:
            return

        loop = asyncio.get_event_loop()
        while True:
            try:
                # Читаем по одному символу для более быстрого получения данных
                # или используем readline для построчного чтения
                line = await loop.run_in_executor(None, stream.readline)
                if not line:
                    # Проверяем, завершен ли процесс
                    if self.process and self.process.poll() is not None:
                        # Читаем оставшиеся данные
                        remaining = stream.read()
                        if remaining:
                            self._add_log(remaining, stream_type, callback)
                        break
                    # Если процесс еще работает, продолжаем ждать
                    await asyncio.sleep(0.1)
                    continue
                # Добавляем строку через _add_log, чтобы она попала в ordered_logs
                self._add_log(line, stream_type, callback)
            except Exception as e:
                # Логируем ошибку, но не прерываем выполнение
                error_msg = f"[ERROR] Stream read error: {str(e)}\n"
                self._add_log(error_msg, stream_type, callback)
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
    triggered_by: Optional[str] = None,
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
        triggered_by=triggered_by or "manual",
    )
    db.add(deployment)
    db.commit()
    db.refresh(deployment)
    deployment_id = deployment.id

    # Если есть активный деплой, просто возвращаем ID (деплой в очереди)
    if active_deployment:
        return deployment_id

    # Запускаем деплой сразу
    await _run_deployment_async(deployment_id, service, command, triggered_by)

    return deployment_id


async def _run_deployment_async(deployment_id: int, service: Service, command: str, triggered_by: Optional[str] = None):
    """Запустить деплой асинхронно."""
    from pushbot.database import SessionLocal
    
    # Создаем новую сессию БД для фоновой задачи
    async def run_deployment():
        db_session = SessionLocal()
        try:
            runner = DeploymentRunner(db_session, deployment_id)
            runner.triggered_by = triggered_by
            active_deployments[deployment_id] = runner
            try:
                await runner.run(command, service.path, triggered_by=triggered_by)
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
    triggered_by = queued_deployment.triggered_by or "manual"
    await _run_deployment_async(queued_deployment.id, service, service.deploy_command, triggered_by)


def get_active_deployment(deployment_id: int) -> Optional[DeploymentRunner]:
    """Получить активный деплой по ID."""
    return active_deployments.get(deployment_id)


def get_all_active_deployments() -> dict[int, DeploymentRunner]:
    """Получить все активные деплои."""
    return active_deployments.copy()
