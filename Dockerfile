FROM golang:1.26-alpine AS builder

RUN apk add --no-cache git

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/storage-service ./cmd/server

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata

COPY --from=builder /bin/storage-service /bin/storage-service

EXPOSE 19093 18083

# Args pass through to the binary:
#   docker run <image>            -> /bin/storage-service         (start server)
#   docker run <image> migrate    -> /bin/storage-service migrate (run migrations)
ENTRYPOINT ["/bin/storage-service"]
