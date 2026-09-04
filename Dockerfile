# Build.
#
# Pinned above the floor go.mod declares, never below it. That floor is not a
# preference: the memory accounting is calibrated against real heap growth and
# does not hold under Go 1.21, where a -maxmemory bound under-counts by up to
# 39% and stops bounding - see internal/data_structure/memory.go. An image built
# on an older toolchain would be quietly wrong in exactly that way.
FROM golang:1.24-alpine AS build

WORKDIR /src

# Dependencies first, so a change to the source does not re-download them.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=docker
RUN CGO_ENABLED=0 go build \
      -ldflags "-s -w -X github.com/brandopakel/keel/internal/config.Version=${VERSION}" \
      -o /out/keel ./cmd/keel

# The data directory is made here, owned by the user that will run, because
# scratch has no shell to make it later and a WORKDIR Docker creates implicitly
# belongs to root. Getting this wrong costs nothing until someone passes
# -appendonly, at which point the server cannot open its own log.
RUN mkdir -p /out/data && chown 65534:65534 /out/data

# Run.
#
# Static binary, so the image needs nothing else. scratch rather than alpine:
# there is no shell to exec into, which is a smaller thing to defend than a
# server that has no authentication of its own.
FROM scratch

COPY --from=build /out/keel /keel
COPY --from=build --chown=65534:65534 /out/data /data

# Unprivileged. The number is used rather than a name because scratch has no
# /etc/passwd to resolve one.
USER 65534:65534

EXPOSE 8081

# The append-only log is written relative to the working directory, so a volume
# mounted here is what survives the container. Created above rather than left
# to WORKDIR, so it belongs to the user that has to write into it.
WORKDIR /data
VOLUME ["/data"]

ENTRYPOINT ["/keel"]
CMD ["-host", "0.0.0.0", "-port", "8081"]
