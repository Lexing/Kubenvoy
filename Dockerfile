FROM golang:1.10-alpine3.8

ADD . /go/src/kubenvoyxds

WORKDIR /go/src/kubenvoyxds/main

RUN go build -o main main.go

ENTRYPOINT ["./main"]


