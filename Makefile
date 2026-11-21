.PHONY: test vet build install bundle integration

test:
	go test -race ./...

vet:
	go vet ./...
	for script in scripts/*.sh; do bash -n "$$script"; done

build:
	go build ./...

install:
	./scripts/install-server.sh

bundle:
	./scripts/build-bundle.sh

integration:
	./scripts/test-vz-public.sh
