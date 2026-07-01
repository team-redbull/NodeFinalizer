# Build stage
FROM docker.io/library/golang:1.21-alpine AS builder

WORKDIR /workspace

COPY go.mod go.sum ./
COPY vendor/ vendor/
COPY cmd/ cmd/
COPY pkg/ pkg/

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -mod=vendor -a -o webhook ./cmd/webhook

# Final stage
FROM gcr.io/distroless/static:nonroot

WORKDIR /
COPY --from=builder /workspace/webhook .

USER 65532:65532

ENTRYPOINT ["/webhook"]
