test:
	@go test -v -failfast -cover -coverprofile=cover.out -race
	@go tool cover -html=cover.out
