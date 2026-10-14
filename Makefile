# Check and cleanup; scripts/ builds and publishes the app.
#
#   make check		# formatting, lint, known vulnerabilities, security
#   make clean		# remove what builds leave: build/ and the binaries

# Everything builds without cgo, as MyGo requires.
export CGO_ENABLED := 0

# Where scripts/ and go tool mygo build put the apps, disk images and
# updates; and the binaries go build . and go build ./cmd/... leave here.
BUILD_DIR ?= build
BINARIES := unscan fetchdeps

.PHONY: check clean

check:
	@for tool in golangci-lint govulncheck gosec; do \
		command -v $$tool >/dev/null || { echo "check needs $$tool on PATH" >&2; exit 1; }; \
	done
	@unformatted=$$(gofmt -l .); \
		if [ -n "$$unformatted" ]; then echo "gofmt would change:" >&2; echo "$$unformatted" >&2; exit 1; fi
	golangci-lint run --no-config ./...
	govulncheck ./...
	gosec -quiet -tests -exclude-generated ./...

clean:
	@case "$(strip $(BUILD_DIR))" in \
		"" | / | . | ..) echo 'BUILD_DIR must name a build directory.' >&2; exit 1 ;; \
	esac
	rm -rf -- "$(BUILD_DIR)"
	rm -f -- $(BINARIES)
