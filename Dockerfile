# Build stage
FROM golang:1.24.0 AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o bin/distributor ./cmd/distributor/main.go && \
    CGO_ENABLED=0 GOOS=linux go build -o bin/worker ./cmd/worker/main.go && \
    CGO_ENABLED=0 GOOS=linux go build -o bin/cli ./cmd/cli/main.go

# Runtime stage
FROM alpine:3.20

RUN apk add --no-cache bash ca-certificates

WORKDIR /app

COPY --from=builder /app/bin/ ./bin/
COPY entrypoint.sh .
RUN chmod +x entrypoint.sh

ENV RUN_AS=distributor

ENTRYPOINT ["./entrypoint.sh"]
