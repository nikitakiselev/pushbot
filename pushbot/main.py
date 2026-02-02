"""Главный файл FastAPI приложения."""
import asyncio
from contextlib import asynccontextmanager
from fastapi import FastAPI, Request, Depends, HTTPException
from fastapi.responses import HTMLResponse, StreamingResponse
from sqlalchemy.orm import Session
from sqlalchemy import desc
import json

from pushbot.database import get_db, init_db
from pushbot.config import load_config, AppConfig
from pushbot.models import Service, Deployment
from pushbot.webhook import handle_github_webhook
from pushbot.deployer import get_all_active_deployments, get_active_deployment

# Загружаем конфигурацию при старте
app_config: AppConfig = None


@asynccontextmanager
async def lifespan(app: FastAPI):
    """Инициализация при старте и очистка при остановке."""
    global app_config
    # Инициализация базы данных
    init_db()
    # Загрузка конфигурации
    app_config = load_config()
    # Синхронизация сервисов из конфигурации с базой данных
    db = next(get_db())
    try:
        # Получаем список имен сервисов из конфигурации
        config_service_names = {s.name for s in app_config.services}
        
        # Получаем все сервисы из базы данных
        db_services = db.query(Service).all()
        
        # Удаляем сервисы, которых нет в конфигурации
        # Сначала удаляем связанные деплои, чтобы избежать нарушения ограничений
        for db_service in db_services:
            if db_service.name not in config_service_names:
                # Удаляем все деплои, связанные с этим сервисом
                db.query(Deployment).filter(Deployment.service_id == db_service.id).delete()
                # Теперь можно безопасно удалить сервис
                db.delete(db_service)
        
        # Добавляем или обновляем сервисы из конфигурации
        for service_config in app_config.services:
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
            else:
                # Обновляем конфигурацию
                service.repository = service_config.repository
                service.path = service_config.path
                service.branch = service_config.branch
                service.deploy_command = service_config.deploy_command
        db.commit()
    finally:
        db.close()
    yield
    # Очистка при остановке (если необходимо)


app = FastAPI(title="PushBot", lifespan=lifespan)

from jinja2 import Environment, FileSystemLoader

class Jinja2Templates:
    def __init__(self, directory: str):
        self.env = Environment(loader=FileSystemLoader(directory))
    
    def TemplateResponse(self, template_name: str, context: dict):
        template = self.env.get_template(template_name)
        content = template.render(**context)
        return HTMLResponse(content=content)


templates = Jinja2Templates("pushbot/templates")


@app.get("/", response_class=HTMLResponse)
async def index(request: Request, db: Session = Depends(get_db)):
    """Главная страница с мониторингом деплоев."""
    # Получаем активные деплои (running и queued)
    active_deployments_list = []
    # Сначала добавляем running деплои
    for deployment_id, runner in get_all_active_deployments().items():
        deployment = db.query(Deployment).filter(Deployment.id == deployment_id).first()
        if deployment:
            active_deployments_list.append({
                "id": deployment.id,
                "service_id": deployment.service_id,
                "status": deployment.status,
                "started_at": deployment.started_at.isoformat() if deployment.started_at else None,
                "finished_at": deployment.finished_at.isoformat() if deployment.finished_at else None,
                "exit_code": deployment.exit_code,
                "commit_sha": deployment.commit_sha,
                "commit_message": deployment.commit_message,
                "branch": deployment.branch,
                "stdout": deployment.stdout,
                "stderr": deployment.stderr,
            })
    # Добавляем queued деплои
    queued_deployments = db.query(Deployment).filter(Deployment.status == "queued").all()
    for deployment in queued_deployments:
        active_deployments_list.append({
            "id": deployment.id,
            "service_id": deployment.service_id,
            "status": deployment.status,
            "started_at": deployment.started_at.isoformat() if deployment.started_at else None,
            "finished_at": deployment.finished_at.isoformat() if deployment.finished_at else None,
            "exit_code": deployment.exit_code,
            "commit_sha": deployment.commit_sha,
            "commit_message": deployment.commit_message,
            "branch": deployment.branch,
            "stdout": deployment.stdout,
            "stderr": deployment.stderr,
        })

    # Получаем последние завершенные деплои
    recent_deployments_db = (
        db.query(Deployment)
        .filter(Deployment.status.in_(["success", "failed"]))
        .order_by(desc(Deployment.finished_at))
        .limit(20)
        .all()
    )
    
    recent_deployments = [
        {
            "id": d.id,
            "service_id": d.service_id,
            "status": d.status,
            "started_at": d.started_at.isoformat() if d.started_at else None,
            "finished_at": d.finished_at.isoformat() if d.finished_at else None,
            "exit_code": d.exit_code,
            "commit_sha": d.commit_sha,
            "commit_message": d.commit_message,
            "branch": d.branch,
            "stdout": d.stdout,
            "stderr": d.stderr,
        }
        for d in recent_deployments_db
    ]

    # Получаем все сервисы
    services = db.query(Service).all()
    
    # Преобразуем сервисы в JSON для Vue.js
    services_json = json.dumps([
        {
            "id": s.id,
            "name": s.name,
            "repository": s.repository,
            "path": s.path,
            "branch": s.branch,
            "deploy_command": s.deploy_command,
        }
        for s in services
    ])

    return templates.TemplateResponse(
        "index.html",
        {
            "request": request,
            "active_deployments": active_deployments_list,
            "recent_deployments": recent_deployments,
            "services": services,
            "services_json": services_json,
        },
    )


async def _handle_webhook_request(request: Request, db: Session):
    """Общая функция для обработки webhook запросов."""
    import os
    import json
    from pushbot.webhook import verify_github_signature
    
    # Проверяем Content-Type заголовок
    content_type = request.headers.get("Content-Type", "")
    if "application/json" not in content_type:
        raise HTTPException(
            status_code=400,
            detail=f"Неверный Content-Type. Ожидается application/json, получен: {content_type}"
        )
    
    # Получаем секрет из переменной окружения
    webhook_secret = os.getenv("GITHUB_WEBHOOK_SECRET")
    
    # Получаем тело запроса как bytes для проверки подписи
    body_bytes = await request.body()
    
    if not body_bytes:
        raise HTTPException(status_code=400, detail="Тело запроса пусто")
    
    # Проверяем подпись, если секрет задан
    if webhook_secret:
        signature_header = request.headers.get("X-Hub-Signature-256")
        if not verify_github_signature(body_bytes, signature_header, webhook_secret):
            raise HTTPException(
                status_code=401,
                detail="Неверная подпись webhook. Проверьте GITHUB_WEBHOOK_SECRET."
            )
    
    # Парсим JSON из тела запроса
    try:
        payload = json.loads(body_bytes.decode('utf-8'))
    except UnicodeDecodeError as e:
        raise HTTPException(
            status_code=400,
            detail=f"Ошибка декодирования UTF-8: {str(e)}"
        )
    except json.JSONDecodeError as e:
        raise HTTPException(
            status_code=400,
            detail=f"Неверный формат JSON: {str(e)}"
        )
    
    # Проверяем, что payload является словарем
    if not isinstance(payload, dict):
        raise HTTPException(
            status_code=400,
            detail="Payload должен быть JSON объектом (словарем)"
        )

    result = await handle_github_webhook(db, app_config, payload)
    if "error" in result:
        raise HTTPException(status_code=400, detail=result["error"])

    return result


@app.post("/")
async def webhook_root(request: Request, db: Session = Depends(get_db)):
    """Эндпоинт для приема вебхуков от GitHub на корневом пути."""
    return await _handle_webhook_request(request, db)


@app.post("/webhook")
async def webhook(request: Request, db: Session = Depends(get_db)):
    """Эндпоинт для приема вебхуков от GitHub."""
    return await _handle_webhook_request(request, db)


@app.get("/api/deployments/active")
async def get_active_deployments(db: Session = Depends(get_db)):
    """API для получения списка активных деплоев (running и queued)."""
    active_deployments_list = []
    # Сначала добавляем running деплои
    for deployment_id, runner in get_all_active_deployments().items():
        deployment = db.query(Deployment).filter(Deployment.id == deployment_id).first()
        if deployment:
            service = db.query(Service).filter(Service.id == deployment.service_id).first()
            active_deployments_list.append({
                "id": deployment.id,
                "service_id": deployment.service_id,
                "service_name": service.name if service else "Unknown",
                "status": deployment.status,
                "started_at": deployment.started_at.isoformat() if deployment.started_at else None,
                "finished_at": deployment.finished_at.isoformat() if deployment.finished_at else None,
                "exit_code": deployment.exit_code,
                "commit_sha": deployment.commit_sha,
                "commit_message": deployment.commit_message,
                "branch": deployment.branch,
                "stdout": deployment.stdout,
                "stderr": deployment.stderr,
            })
    # Добавляем queued деплои
    queued_deployments = db.query(Deployment).filter(Deployment.status == "queued").all()
    for deployment in queued_deployments:
        service = db.query(Service).filter(Service.id == deployment.service_id).first()
        active_deployments_list.append({
            "id": deployment.id,
            "service_id": deployment.service_id,
            "service_name": service.name if service else "Unknown",
            "status": deployment.status,
            "started_at": deployment.started_at.isoformat() if deployment.started_at else None,
            "finished_at": deployment.finished_at.isoformat() if deployment.finished_at else None,
            "exit_code": deployment.exit_code,
            "commit_sha": deployment.commit_sha,
            "commit_message": deployment.commit_message,
            "branch": deployment.branch,
            "stdout": deployment.stdout,
            "stderr": deployment.stderr,
        })
    return {"active_deployments": active_deployments_list}


@app.get("/api/deployments/{deployment_id}/logs")
async def get_deployment_logs(deployment_id: int, db: Session = Depends(get_db)):
    """API для получения логов деплоя в реальном времени (SSE)."""
    deployment = db.query(Deployment).filter(Deployment.id == deployment_id).first()
    if not deployment:
        raise HTTPException(status_code=404, detail="Деплой не найден")

    async def generate_logs():
        """Генератор для Server-Sent Events."""
        runner = get_active_deployment(deployment_id)
        if runner:
            # Если деплой активен, отправляем логи из упорядоченного буфера
            sorted_logs = sorted(runner.ordered_logs, key=lambda x: x[0])
            for _, line, stream_type in sorted_logs:
                yield f"data: {json.dumps({'type': stream_type, 'line': line}, ensure_ascii=False)}\n\n"
            
            # Отправляем обновления в реальном времени
            last_logs_count = len(runner.ordered_logs)
            while True:
                await asyncio.sleep(0.5)
                
                # Проверяем новые строки в упорядоченном буфере
                if len(runner.ordered_logs) > last_logs_count:
                    # Сортируем только новые логи и отправляем их
                    new_logs = runner.ordered_logs[last_logs_count:]
                    new_logs_sorted = sorted(new_logs, key=lambda x: x[0])
                    for _, line, stream_type in new_logs_sorted:
                        yield f"data: {json.dumps({'type': stream_type, 'line': line}, ensure_ascii=False)}\n\n"
                    last_logs_count = len(runner.ordered_logs)
                
                # Проверяем, завершен ли деплой
                from pushbot.database import SessionLocal
                check_db = SessionLocal()
                try:
                    check_deployment = check_db.query(Deployment).filter(Deployment.id == deployment_id).first()
                    if check_deployment and check_deployment.status != "running":
                        # Отправляем финальный статус
                        yield f"data: {json.dumps({'type': 'status', 'status': check_deployment.status, 'exit_code': check_deployment.exit_code}, ensure_ascii=False)}\n\n"
                        break
                finally:
                    check_db.close()
        else:
            # Если деплой завершен, отправляем сохраненные логи, отсортированные по времени
            import re
            from datetime import datetime
            
            all_logs = []
            newline = '\n'
            
            def parse_timestamp_from_line(line: str) -> datetime:
                """Извлекает временную метку из строки лога."""
                match = re.match(r'\[(\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}(?:\.\d{1,3})?)\]', line)
                if match:
                    timestamp_str = match.group(1)
                    try:
                        if '.' in timestamp_str:
                            # Нормализуем миллисекунды до 6 знаков (микросекунды)
                            parts = timestamp_str.split('.')
                            if len(parts) == 2:
                                ms = parts[1].ljust(6, '0')[:6]
                                timestamp_str_normalized = f"{parts[0]}.{ms}"
                            else:
                                timestamp_str_normalized = timestamp_str
                            return datetime.strptime(timestamp_str_normalized, "%Y-%m-%d %H:%M:%S.%f")
                        else:
                            return datetime.strptime(timestamp_str, "%Y-%m-%d %H:%M:%S")
                    except ValueError:
                        return datetime.min
                return datetime.min
            
            # Парсим все логи из stdout и stderr, объединяем и сортируем
            if deployment.stdout:
                for line in deployment.stdout.split("\n"):
                    if line.strip():
                        timestamp = parse_timestamp_from_line(line)
                        line_with_newline = line + newline if not line.endswith('\n') else line
                        all_logs.append((timestamp, 'stdout', line_with_newline))
            
            if deployment.stderr:
                for line in deployment.stderr.split("\n"):
                    if line.strip():
                        timestamp = parse_timestamp_from_line(line)
                        line_with_newline = line + newline if not line.endswith('\n') else line
                        all_logs.append((timestamp, 'stderr', line_with_newline))
            
            # Сортируем только по точному времени (миллисекунды уже включены)
            all_logs.sort(key=lambda x: x[0])
            for _, log_type, log_line in all_logs:
                yield f"data: {json.dumps({'type': log_type, 'line': log_line}, ensure_ascii=False)}\n\n"
            
            # Отправляем финальный статус
            yield f"data: {json.dumps({'type': 'status', 'status': deployment.status, 'exit_code': deployment.exit_code}, ensure_ascii=False)}\n\n"

    return StreamingResponse(
        generate_logs(),
        media_type="text/event-stream",
        headers={
            "Cache-Control": "no-cache",
            "Connection": "keep-alive",
            "X-Accel-Buffering": "no",
        },
    )


@app.get("/api/deployments/{deployment_id}")
async def get_deployment(deployment_id: int, db: Session = Depends(get_db)):
    """API для получения информации о деплое."""
    deployment = db.query(Deployment).filter(Deployment.id == deployment_id).first()
    if not deployment:
        raise HTTPException(status_code=404, detail="Деплой не найден")

    service = db.query(Service).filter(Service.id == deployment.service_id).first()
    
    return {
        "id": deployment.id,
        "service_name": service.name if service else "Unknown",
        "status": deployment.status,
        "started_at": deployment.started_at.isoformat() if deployment.started_at else None,
        "finished_at": deployment.finished_at.isoformat() if deployment.finished_at else None,
        "exit_code": deployment.exit_code,
        "commit_sha": deployment.commit_sha,
        "commit_message": deployment.commit_message,
        "branch": deployment.branch,
        "stdout": deployment.stdout,
        "stderr": deployment.stderr,
    }


@app.get("/api/deployments")
async def get_deployments(
    limit: int = 50,
    status: str = None,
    db: Session = Depends(get_db),
):
    """API для получения списка деплоев."""
    query = db.query(Deployment)
    if status:
        query = query.filter(Deployment.status == status)
    deployments = query.order_by(desc(Deployment.started_at)).limit(limit).all()

    result = []
    for deployment in deployments:
        service = db.query(Service).filter(Service.id == deployment.service_id).first()
        result.append({
            "id": deployment.id,
            "service_id": deployment.service_id,
            "service_name": service.name if service else "Unknown",
            "status": deployment.status,
            "started_at": deployment.started_at.isoformat() if deployment.started_at else None,
            "finished_at": deployment.finished_at.isoformat() if deployment.finished_at else None,
            "exit_code": deployment.exit_code,
            "commit_sha": deployment.commit_sha,
            "commit_message": deployment.commit_message,
            "branch": deployment.branch,
            "stdout": deployment.stdout,
            "stderr": deployment.stderr,
        })

    return {"deployments": result}


@app.get("/api/services")
async def get_services(db: Session = Depends(get_db)):
    """API для получения списка сервисов."""
    services = db.query(Service).all()
    result = [
        {
            "id": s.id,
            "name": s.name,
            "repository": s.repository,
            "path": s.path,
            "branch": s.branch,
            "deploy_command": s.deploy_command,
        }
        for s in services
    ]
    return {"services": result}


@app.post("/api/services/{service_id}/deploy")
async def deploy_service(service_id: int, db: Session = Depends(get_db)):
    """API для ручного запуска деплоя сервиса."""
    from pushbot.deployer import start_deployment
    
    # Получаем сервис из базы данных
    service = db.query(Service).filter(Service.id == service_id).first()
    if not service:
        raise HTTPException(status_code=404, detail="Сервис не найден")
    
    # Запускаем деплой
    deployment_id = await start_deployment(
        db=db,
        service=service,
        command=service.deploy_command,
        commit_sha=None,
        commit_message="Ручной запуск деплоя",
        branch=service.branch,
        triggered_by="manual",
    )
    
    return {
        "message": "Деплой запущен",
        "deployment_id": deployment_id,
        "service": service.name,
    }


@app.post("/api/deployments/clear")
async def clear_deployments(db: Session = Depends(get_db)):
    """API для очистки завершенных деплоев (success и failed)."""
    # Удаляем только завершенные деплои (success и failed), не трогая активные (running, queued)
    deleted_count = db.query(Deployment).filter(
        Deployment.status.in_(["success", "failed"])
    ).delete()
    
    db.commit()
    
    return {
        "message": f"Удалено завершенных деплоев: {deleted_count}",
        "deleted_count": deleted_count
    }


if __name__ == "__main__":
    import uvicorn
    uvicorn.run(app, host="0.0.0.0", port=8009)
