FROM golang:1.18.1-alpine

RUN apk update && apk add git

ENV CGO_ENABLED 0

WORKDIR /go/src

ADD . /go/src
