test:
	@go test -v -failfast -cover -coverprofile=cover.out
	@go tool cover -html=cover.out
