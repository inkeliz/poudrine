# Build and package on FreeBSD with base-system make and tar.
GO ?= go
TAR ?= tar
DESTDIR ?=
LOCALBASE = /usr/local
release_files = poudrine poudrine-status rc.d examples/poudrine.json Makefile README.md
source_files = *.go go.mod go.sum cmd internal rc.d examples Makefile README.md

.PHONY: all test build install install-built dist
all: build

test:
	CGO_ENABLED=0 ${GO} test ./...

build:
	CGO_ENABLED=0 GOOS=freebsd ${GO} build -trimpath -o poudrine ./cmd/poudrine
	CGO_ENABLED=0 GOOS=freebsd ${GO} build -trimpath -o poudrine-status ./cmd/poudrine-status

# Binary distributions contain only the executables and installation files.
install:
	@if test -f go.mod; then ${MAKE} build; fi
	@${MAKE} install-built

install-built:
	test -x poudrine && test -x poudrine-status
	install -d "${DESTDIR}${LOCALBASE}/sbin" "${DESTDIR}${LOCALBASE}/etc/rc.d"
	install -S -m 0555 poudrine poudrine-status "${DESTDIR}${LOCALBASE}/sbin/"
	install -S -m 0555 rc.d/poudrine rc.d/poudrine_status "${DESTDIR}${LOCALBASE}/etc/rc.d/"

dist: build
	mkdir -p dist
	${TAR} -czf "dist/poudrine-freebsd-$$(${GO} env GOARCH).tar.gz" -s ',^,poudrine/,' ${release_files}
	${TAR} -czf dist/poudrine-src.tar.gz -s ',^,poudrine/,' ${source_files}
