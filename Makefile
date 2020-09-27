.PHONY: build
build:
	go build -o variant ./pkg/cmd

bin/goimports:
	GOBIN=$(PWD)/bin go install golang.org/x/tools/cmd/goimports

.PHONY: fmt
fmt: bin/goimports
	bin/goimports -w pkg .
	gofmt -w -s pkg .

.PHONY: test
test: build
	go vet ./...
	PATH=$(PWD):$(PATH) go test -race -v ./...

bin/golangci-lint:
	curl -sfL https://install.goreleaser.com/github.com/golangci/golangci-lint.sh | sh -s v1.23.1

.PHONY: lint
lint: bin/golangci-lint
	bin/golangci-lint run --tests \
	  --enable-all \
	  --disable gochecknoglobals \
	  --disable gochecknoinits \
	  --disable gomnd,funlen,prealloc,gocritic,lll,gocognit

.PHONY: smoke
smoke: export GOBIN=$(shell pwd)/tools
smoke: build
	go get github.com/rakyll/statik

	make build
	rm -rf build/simple
	PATH=${PATH}:$(GOBIN) ./variant export go examples/simple build/simple
	cd build/simple; go build -o simple ./
	build/simple/simple -h | tee smoke.log
	grep "Namespace to interact with" smoke.log

	rm build/simple/simple
	PATH=${PATH}:$(GOBIN) ./variant export binary examples/simple build/simple
	build/simple/simple -h | tee smoke2.log
	grep "Namespace to interact with" smoke2.log

	rm -rf build/import-multi
	VARIANT_BUILD_VER=v0.0.0 VARIANT_BUILD_REPLACE=$(shell pwd) PATH=${PATH}:$(GOBIN) ./variant export binary examples/advanced/import-multi build/import-multi
	build/import-multi foo baz HELLO > build/import-multi.log
	bash -c 'diff <(echo HELLO) <(cat build/import-multi.log)'
