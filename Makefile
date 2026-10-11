.PHONY: build test check examples clean

build:
	go build -o nqc .

test: build
	go vet ./...
	go test -v -timeout 600s ./...

check: build
	for f in examples/*.nq examples/os/*.nq tests/qemu/*.nq; do ./nqc check $$f || exit 1; done

examples: build
	for f in hello ptr arrays structs syscalls; do ./nqc run examples/$$f.nq || exit 1; done

clean:
	rm -f nqc
	find examples tests -type f ! -name '*.nq' -delete
