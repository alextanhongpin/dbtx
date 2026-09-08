args := -failfast -cover -coverprofile=cover.out -race
test:
	@find . -name go.mod -execdir go test $(args) ./... \;
	@go tool cover -html=cover.out

lint:
	@find . -name '*.sql' -exec go tool sqlfmt -w {} \;

sqlc:
	@find . -name 'sqlc.yaml' -execdir sqlc generate  \;

install:
	go get -tool github.com/dimitri/sqlfmt/cmd/sqlfmt
	go get -tool github.com/sqlc-dev/sqlc/cmd/sqlc
