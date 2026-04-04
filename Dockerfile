FROM golang:1.22-alpine AS builder
RUN apk add --no-cache git
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.Version=$(git describe --tags --always)" -o fusionride ./cmd/fusionride

FROM alpine:3.19
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /app
COPY --from=builder /app/fusionride .
COPY config/config.yaml /app/config/config.yaml
EXPOSE 8096
VOLUME ["/app/data", "/app/config"]
ENTRYPOINT ["./fusionride"]
