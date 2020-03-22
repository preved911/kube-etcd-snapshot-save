FROM golang:1.13 as build

WORKDIR /app
COPY go.mod go.sum main.go ./
RUN go mod download
RUN go build -o etcd-snapshot-save .

FROM alpine
COPY --from=build /app/etcd-snapshot-save /usr/local/bin/etcd-snapshot-save
RUN apk update \
    && apk add tzdata \
    && mkdir /lib64 \
    && ln -s /lib/libc.musl-x86_64.so.1 /lib64/ld-linux-x86-64.so.2
ENTRYPOINT ["/usr/local/bin/etcd-snapshot-save"]
