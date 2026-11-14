PSQL ?= psql
DATABASE_URL ?= postgres://chainlens:chainlens_dev@localhost:5432/chainlens?sslmode=disable
MIGRATIONS := $(sort $(wildcard migrations/*.sql))

.PHONY: build vet fmt fmt-check migrate db-reset probe

build:
	go build ./...

vet:
	go vet ./...

fmt:
	gofumpt -w .

fmt-check:
	@test -z "$$(gofumpt -l .)" || { echo "gofumpt 需要重排下列文件:"; gofumpt -l .; exit 1; }

# 按文件名顺序依次应用 migration。执行不可重复：重跑 0001 会因表已存在而失败。
# 本地需要重建时用 db-reset。
migrate:
	@for file in $(MIGRATIONS); do \
		echo "applying $$file"; \
		$(PSQL) "$(DATABASE_URL)" -v ON_ERROR_STOP=1 -f $$file || exit 1; \
	done

# 本地反复验证用：清空 public schema 后重新应用 migration，仅限开发库。
db-reset:
	$(PSQL) "$(DATABASE_URL)" -v ON_ERROR_STOP=1 -c "DROP SCHEMA public CASCADE; CREATE SCHEMA public;"
	$(MAKE) migrate

probe:
	go run ./cmd/probe
