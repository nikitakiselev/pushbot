#!/usr/bin/env python3
"""Скрипт для запуска PushBot сервера."""
import sys
import os
import uvicorn

def check_venv():
    """Проверка, что виртуальное окружение активировано."""
    # Проверяем, запущены ли мы в venv
    in_venv = (
        hasattr(sys, 'real_prefix') or
        (hasattr(sys, 'base_prefix') and sys.base_prefix != sys.prefix)
    )
    
    if not in_venv:
        venv_path = os.path.join(os.path.dirname(__file__), 'venv')
        if os.path.exists(venv_path):
            print("⚠️  Внимание: виртуальное окружение не активировано!")
            print(f"   Активируйте его командой: source {venv_path}/bin/activate")
            print("   Или используйте скрипт setup.sh для автоматической настройки")
            print()
            response = input("Продолжить без venv? (y/N): ")
            if response.lower() != 'y':
                sys.exit(1)

if __name__ == "__main__":
    check_venv()
    uvicorn.run(
        "pushbot.main:app",
        host="0.0.0.0",
        port=8009,
        reload=False,
        log_level="info",
    )
