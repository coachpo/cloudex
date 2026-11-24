.PHONY: test build check

test:
	cd apps/api && go test ./...

build:
	cd apps/web && npm ci && npm run build

check: test
	cd apps/api && go vet ./...
	cd apps/web && npm run typecheck && node --test tests/*.test.mjs
