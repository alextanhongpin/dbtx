.PHONY: test vet sqlc
test:
	go test -race -count=1 ./...
vet:
	go vet ./...
sqlc:
	cd ../.. && go tool sqlc generate -f postgres/bandit/repository/sqlc.yaml
