VERSION=v0.1.0

build:
	CGO_ENABLED=0 go build -o prometheus-example-app --installsuffix cgo main.go
build_docker:
	docker build -t quay.io/brancz/prometheus-example-app:$(VERSION) .
