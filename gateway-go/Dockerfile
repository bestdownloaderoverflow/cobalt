FROM golang:1.22-alpine AS builder

WORKDIR /app
COPY go.mod ./
RUN go mod download
COPY main.go ./
RUN go build -o gateway .

FROM alpine:3.19

WORKDIR /app
COPY --from=builder /app/gateway .

CMD ["./gateway"]
