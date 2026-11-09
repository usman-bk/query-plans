.PHONY: db bench test

DATABASE_URL ?= postgres://localhost/plans?sslmode=disable
export DATABASE_URL

# About five and a half seconds through COPY. Minutes row by row, which is
# the difference between a suite anybody runs and one nobody does.
db:
	createdb plans || true
	go run ./cmd/planbench -load

bench:
	go run ./cmd/planbench -v

test:
	go test -race ./...
