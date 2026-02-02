.PHONY: build clean install

build:
	@echo "Building pushbot binary..."
	@pip install pyinstaller > /dev/null 2>&1 || true
	@pyinstaller --name pushbot \
		--onefile \
		--add-data "pushbot/templates:pushbot/templates" \
		--hidden-import=uvicorn \
		--hidden-import=fastapi \
		--hidden-import=sqlalchemy \
		--hidden-import=jinja2 \
		--hidden-import=yaml \
		--hidden-import=pydantic \
		--hidden-import=pydantic_settings \
		--hidden-import=aiofiles \
		--hidden-import=python_multipart \
		--collect-all uvicorn \
		--collect-all fastapi \
		--collect-all starlette \
		pushbot/cli.py
	@echo "Build complete! Binary is in dist/pushbot"

clean:
	rm -rf build dist *.spec
	find . -type d -name __pycache__ -exec rm -r {} + 2>/dev/null || true
	find . -type f -name "*.pyc" -delete

install:
	pip install -r requirements.txt
