FROM golang:1.23.2-bookworm AS builder 

COPY . /build/

WORKDIR /build

RUN git config --global --add safe.directory /build && go mod tidy &&  go build -o http_proxy

FROM debian:bookworm-slim

COPY --from=builder /build/http_proxy /usr/local/bin/http_proxy
COPY backends.yaml /etc/backends.yaml
RUN chmod +x /usr/local/bin/http_proxy

CMD ["/usr/local/bin/http_proxy"]
