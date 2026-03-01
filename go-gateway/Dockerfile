FROM golang:1.23-alpine AS builder

WORKDIR /app

# Install git for go mod download
RUN apk add --no-cache git

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY main.go ./

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o gateway .

FROM alpine:3.20

RUN apk add --no-cache ca-certificates

COPY --from=builder /app/gateway /usr/local/bin/gateway

EXPOSE 9111

CMD ["gateway"]
