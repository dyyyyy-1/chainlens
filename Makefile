PSQL ?= psql
DATABASE_URL ?= postgres://chainlens:chainlens_dev@localhost:5432/chainlens?sslmode=disable
MIGRATIONS := migrations/0001_init.sql

.PHONY: build vet fmt fmt-check migrate db-reset probe

build:
	go build ./...

vet:
	go vet ./...

fmt:
	gofumpt -w .

fmt-check:
	@test -z "$$(gofumpt -l .)" || { echo "gofumpt 需要重排下列文件:"; gofumpt -l .; exit 1; }

# 按序号依次应用 migration。当前只有一个文件，W2 引入版本追踪时再扩展。
migrate:
	$(PSQL) "$(DATABASE_URL)" -v ON_ERROR_STOP=1 -f $(MIGRATIONS)

# 本地反复验证用：清空 public schema 后重新应用 migration，仅限开发库。
db-reset:
	$(PSQL) "$(DATABASE_URL)" -v ON_ERROR_STOP=1 -c "DROP SCHEMA public CASCADE; CREATE SCHEMA public;"
	$(MAKE) migrate

probe:
	go run ./cmd/probe
