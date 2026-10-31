# Include variables from the .envrc file
include .envrc

# ==================================================================================== #
# HELPERS
# ==================================================================================== #

## help: print this help message
.PHONY: help
help:
	@echo 'Usage:'
	@sed -n 's/^##//p' ${MAKEFILE_LIST} | column -t -s ':' | sed -e 's/^/ /'

.PHONY: confirm
confirm:
	@echo -n 'Are you sure? [y/N] ' && read ans && [ $${ans:-N} = y ]

# ==================================================================================== #
# DEVELOPMENT
# ==================================================================================== #

## run/api: run the cmd/api application
.PHONY: run/api
run/api:
	go run ./cmd/api -db-dsn=${GATEWAY_DB_DSN}

## build/api: build the cmd/api binary with the version injected
.PHONY: build/api
build/api:
	go build -ldflags="-s -w -X main.version=$$(git rev-parse --short HEAD)" -o ./bin/api ./cmd/api

## docker/build: build the gateway image with the current git sha as version
.PHONY: docker/build
docker/build:
	docker build --build-arg VERSION=$$(git rev-parse --short HEAD) -t gateway:latest .

## docker/up: build and start the gateway + postgres containers
.PHONY: docker/up
docker/up:
	VERSION=$$(git rev-parse --short HEAD) docker compose up --build -d

## docker/down: stop the gateway + postgres containers
.PHONY: docker/down
docker/down:
	docker compose down

## db/psql: connect to the database using psql
.PHONY: db/psql
db/psql:
	psql ${GATEWAY_DB_DSN}

## db/migrations/new name=$1: create a new database migration
.PHONY: db/migrations/new
db/migrations/new:
	@echo 'Creating migration files for ${name}...'
	migrate create -seq -ext=.sql -dir=./migrations ${name}

## db/migrations/up: apply all up database migrations
.PHONY: db/migrations/up
db/migrations/up: confirm
	@echo 'Running up migrations...'
	migrate -path ./migrations -database ${GATEWAY_DB_DSN} up

# ==================================================================================== #
# QUALITY CONTROL
# ==================================================================================== #

## audit: tidy dependencies and format, vet and test all code
.PHONY: audit
audit: vendor
	@echo 'Formatting code...'
	go fmt ./...
	@echo 'Vetting code...'
	go vet ./...
	staticcheck ./...
	@echo 'Running tests...'
	go test -race -vet=off ./bin/

## vendor: tidy and vendor dependencies
.PHONY: vendor
vendor:
	@echo 'Tidying and verifying module dependencies...'
	go mod tidy
	go mod verify
	@echo 'Vendoring dependencies...'
	go mod vendor
