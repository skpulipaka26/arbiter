FROM golang:1.24-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN go build -ldflags="-w -s" -o arbiter ./cmd/arbiter

FROM alpine:latest

RUN apk --no-cache add ca-certificates
WORKDIR /root/

COPY --from=builder /app/arbiter .
COPY --from=builder /app/config.yaml .

EXPOSE 8080

CMD ["./arbiter", "-config", "config.yaml"]