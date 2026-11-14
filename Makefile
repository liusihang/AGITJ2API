.PHONY: build init run test vet check

build:
	go build -trimpath -o bin/agitj2api .

init: build
	./bin/agitj2api -init

run: build
	./bin/agitj2api

test:
	go test -race -count=1 ./...

vet:
	go vet ./...

check: test vet
	python3 -m compileall -q import_token.py examples scripts
