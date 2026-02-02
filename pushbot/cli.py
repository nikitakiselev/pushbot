"""CLI точка входа для pushbot."""
import sys
import os
import argparse
from pathlib import Path

# Добавляем путь к модулям
sys.path.insert(0, str(Path(__file__).parent.parent))

def get_base_path():
    """Получить базовый путь (для бинарника или исходников)."""
    if getattr(sys, 'frozen', False):
        # Если запущено из бинарника
        return Path(sys.executable).parent
    else:
        # Если запущено из исходников
        return Path(__file__).parent.parent

def find_config(config_name: str = "config.yaml") -> Path:
    """Найти конфигурационный файл."""
    base_path = get_base_path()
    config_path = base_path / config_name
    if not config_path.exists():
        # Пробуем найти config.yaml в текущей директории
        config_path = Path.cwd() / config_name
        if not config_path.exists():
            return None
    return config_path

def cmd_init(args):
    """Команда init - создание файла config.yaml."""
    base_path = get_base_path()
    config_path = base_path / "config.yaml"
    
    if config_path.exists() and not args.force:
        print(f"Error: Config file already exists: {config_path}")
        print("Use --force to overwrite")
        sys.exit(1)
    
    # Создаем пример конфигурации
    config_content = """services:
  - name: example-service
    repository: "owner/repo"
    path: "/path/to/project"
    branch: "master"
    deploy_command: "echo 'Deploy command here'"
"""
    
    with open(config_path, 'w', encoding='utf-8') as f:
        f.write(config_content)
    
    print(f"Config file created: {config_path}")

def cmd_serve(args):
    """Команда serve - запуск веб-сервера."""
    config_path = find_config(args.config)
    if not config_path:
        print(f"Error: Config file not found: {args.config}")
        print("Run 'pushbot init' to create a config file")
        sys.exit(1)
    
    os.environ['PUSHBOT_CONFIG'] = str(config_path)
    
    # Импортируем и запускаем приложение
    import uvicorn
    from pushbot.main import app
    
    uvicorn.run(
        app,
        host=args.host,
        port=args.port,
        log_level="info"
    )

def cmd_deploy(args):
    """Команда deploy - ручной запуск деплоя."""
    config_path = find_config(args.config)
    if not config_path:
        print(f"Error: Config file not found: {args.config}")
        print("Run 'pushbot init' to create a config file")
        sys.exit(1)
    
    os.environ['PUSHBOT_CONFIG'] = str(config_path)
    
    # Импортируем необходимые модули
    import asyncio
    from pushbot.database import SessionLocal, init_db
    from pushbot.config import load_config
    from pushbot.models import Service
    from pushbot.deployer import start_deployment
    
    async def run_deploy():
        # Инициализируем БД и загружаем конфигурацию
        init_db()
        config = load_config()
        
        # Находим сервис
        db = SessionLocal()
        try:
            service = db.query(Service).filter(Service.name == args.service).first()
            if not service:
                print(f"Error: Service '{args.service}' not found")
                print("Available services:")
                for svc in db.query(Service).all():
                    print(f"  - {svc.name}")
                sys.exit(1)
            
            # Запускаем деплой
            deployment_id = await start_deployment(
                db=db,
                service=service,
                command=service.deploy_command,
                commit_sha=None,
                commit_message="Manual deployment from CLI",
                branch=service.branch,
                triggered_by="manual"
            )
            
            print(f"Deployment started: ID={deployment_id}, Service={service.name}")
        finally:
            db.close()
    
    # Запускаем асинхронную функцию
    asyncio.run(run_deploy())

def main():
    """Главная функция CLI."""
    parser = argparse.ArgumentParser(
        description='PushBot - домашний деплой-сервис',
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""
Примеры использования:
  pushbot init                    Создать config.yaml
  pushbot serve                   Запустить веб-сервер
  pushbot serve --port 9000       Запустить на порту 9000
  pushbot deploy my-service       Запустить деплой сервиса
        """
    )
    subparsers = parser.add_subparsers(dest='command', help='Доступные команды', metavar='COMMAND')
    
    # Команда init
    init_parser = subparsers.add_parser('init', help='Создать файл config.yaml')
    init_parser.add_argument(
        '--force',
        action='store_true',
        help='Перезаписать существующий config.yaml'
    )
    
    # Команда serve
    serve_parser = subparsers.add_parser('serve', help='Запустить веб-сервер')
    serve_parser.add_argument(
        '--host',
        default='0.0.0.0',
        help='Хост для веб-сервера (по умолчанию: 0.0.0.0)'
    )
    serve_parser.add_argument(
        '--port',
        type=int,
        default=8009,
        help='Порт для веб-сервера (по умолчанию: 8009)'
    )
    serve_parser.add_argument(
        '--config',
        default='config.yaml',
        help='Путь к конфигурационному файлу (по умолчанию: config.yaml)'
    )
    
    # Команда deploy
    deploy_parser = subparsers.add_parser('deploy', help='Запустить деплой сервиса')
    deploy_parser.add_argument(
        'service',
        help='Название сервиса для деплоя'
    )
    deploy_parser.add_argument(
        '--config',
        default='config.yaml',
        help='Путь к конфигурационному файлу (по умолчанию: config.yaml)'
    )
    
    args = parser.parse_args()
    
    if not args.command:
        parser.print_help()
        sys.exit(0)
    
    if args.command == 'init':
        cmd_init(args)
    elif args.command == 'serve':
        cmd_serve(args)
    elif args.command == 'deploy':
        cmd_deploy(args)
    else:
        parser.print_help()
        sys.exit(1)

if __name__ == '__main__':
    main()
