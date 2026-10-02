args := -v -failfast -cover -coverprofile=cover.out -race
test:
	@go test $(args) ./...
	@go tool cover -html=cover.out

lint:
	@find . -name '*.sql' -exec go tool sqlfmt -w {} \;

sqlc:
	@find . -name 'sqlc.yaml' -execdir sqlc generate  \;

install:
	go get -u -tool github.com/dimitri/sqlfmt/cmd/sqlfmt
	go get -u -tool github.com/sqlc-dev/sqlc/cmd/sqlc
