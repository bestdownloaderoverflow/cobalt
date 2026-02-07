FROM rust:1.83-alpine AS builder

RUN apk add --no-cache musl-dev perl make

WORKDIR /app
COPY Cargo.toml ./
COPY src ./src

RUN cargo build --release

FROM alpine:3.20

RUN apk add --no-cache ca-certificates wget

COPY --from=builder /app/target/release/cobalt-smart-gateway /usr/local/bin/gateway

CMD ["gateway"]
