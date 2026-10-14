SQLC ?= sqlc

.PHONY: sqlc test vet
sqlc:
	$(SQLC) generate -f repository/sqlc.yaml

test:
	go test -race -count=1 -timeout 180s ./...

vet:
	go vet ./...
