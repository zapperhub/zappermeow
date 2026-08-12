# Build stage: compile a static binary with the migrations embedded.
FROM golang:1.25-alpine AS builder

WORKDIR /src

# Dependencies first, so a source-only change reuses the module cache layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO off keeps the binary static, which is what makes distroless viable.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/zappermeow \
    ./cmd/zappermeow

# Runtime stage: no shell, no package manager, no writable system directories.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/zappermeow /usr/local/bin/zappermeow

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/zappermeow"]
CMD ["serve"]
