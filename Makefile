args := -failfast -cover -coverprofile=cover.out -race
test:
	@find . -name go.mod -execdir go test $(args) ./... \;
	@go tool cover -html=cover.out
