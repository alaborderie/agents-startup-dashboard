FROM golang:1.27-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -trimpath \
    -buildvcs=false \
    -ldflags="-s -w" \
    -o /out/dashboard \
    ./cmd/dashboard

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder --chown=65532:65532 /out/dashboard /dashboard

USER 65532:65532
EXPOSE 8080

ENTRYPOINT ["/dashboard"]
