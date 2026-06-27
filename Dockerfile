# Build the studio BFF binary.
FROM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Cache module downloads before copying the source.
COPY go.mod go.mod
COPY go.sum go.sum
RUN go mod download

# Copy the Go source (relies on .dockerignore to filter).
COPY . .

# Build a static binary for the target platform (empty GOARCH => host arch, so
# the image and binary share a platform when built locally).
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o studio ./cmd/studio

# Distroless static base: no shell, runs as an unprivileged user.
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/studio .
USER 65532:65532
EXPOSE 8080

# `serve` is the default subcommand; override CMD to run version/etc.
ENTRYPOINT ["/studio"]
CMD ["serve"]
