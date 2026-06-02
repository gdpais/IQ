FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /bin/worker ./cmd/worker

FROM alpine:3.20
WORKDIR /app
COPY --from=builder /bin/worker /app/worker
ENTRYPOINT ["/app/worker"]
