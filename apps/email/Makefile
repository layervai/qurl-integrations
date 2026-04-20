.PHONY: help deploy-sandbox deploy-production test lint clean package info check-email

help:
	@echo "Qurl Email Bot - Available Commands"
	@echo ""
	@echo "  make check-email           Run lint and tests (CI parity)"
	@echo "  make test                 Run tests"
	@echo "  make lint                 Run linter"
	@echo "  make clean                Clean build artifacts"
	@echo "  make package              Package Lambda function"
	@echo "  make deploy-sandbox        Deploy to sandbox environment (layerv.xyz)"
	@echo "  make deploy-production     Deploy to production environment (layerv.ai)"
	@echo "  make info                 Show environment configuration"
	@echo ""
	@echo "  make deploy-sandbox ARGS='--dry-run'           Preview sandbox deployment plan"
	@echo "  make deploy-sandbox ARGS='--skip-tests'        Skip tests and deploy directly"
	@echo "  make deploy-sandbox ARGS='--skip-package'       Skip packaging (use existing package)"

check-email:
	cd apps/email && pip install -q -r requirements-dev.txt && ruff check . && python -m pytest tests/ -q --tb=short

deploy-sandbox:
	python3 deploy.py --env sandbox $(ARGS)

deploy-production:
	python3 deploy.py --env production $(ARGS)

test:
	cd apps/email && python3 -m pytest tests/ -q --tb=short

lint:
	cd apps/email && python3 -m ruff check .

clean:
	rm -f apps/email/lambda.zip
	rm -rf apps/email/package/
	find apps/email -type d -name __pycache__ -exec rm -rf {} + 2>/dev/null || true
	find apps/email -type f -name "*.pyc" -delete 2>/dev/null || true

package:
	python3 deploy.py --env sandbox --skip-tests --dry-run
	@echo "See --skip-package to use existing package"

info:
	@echo "=== Sandbox Environment ==="
	@echo "  Bot address: qurl@layerv.xyz"
	@echo "  Lambda: qurl-email-bot-sandbox"
	@echo "  SQS: qurl-email-processing-sandbox"
	@echo "  S3: qurl-email-inbound-sandbox"
	@echo "  DynamoDB: qurl-email-dispatch-log-sandbox"
	@echo ""
	@echo "=== Production Environment ==="
	@echo "  Bot address: qurl@layerv.ai"
	@echo "  Lambda: qurl-email-bot"
	@echo "  SQS: qurl-email-processing"
	@echo "  S3: qurl-email-inbound-prod"
	@echo "  DynamoDB: qurl-email-dispatch-log"
