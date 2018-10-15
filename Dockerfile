FROM golang:1.10-alpine3.8 AS builder

ENV CGO_ENABLED=0 
ENV GOOS=linux 
ENV GOARCH=amd64

# This can speed up future builds because of cache, only rebuild when vendors are
# added.
ADD vendor /go/src/kubenvoy/vendor/
RUN go build -i kubenvoy/vendor/...

ADD . /go/src/kubenvoy
WORKDIR /go/src/kubenvoy


RUN go build -v -o /go/bin/main main/main.go

# Restart and only copy the built binary
FROM alpine:3.8
COPY --from=builder /go/bin/main /go/bin/main
ENTRYPOINT ["/go/bin/main"]
