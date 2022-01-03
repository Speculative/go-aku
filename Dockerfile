# syntax=docker/dockerfile:1

FROM golang:1.17.5-alpine
WORKDIR /app

RUN apk add --no-cache ffmpeg

COPY go.mod ./
COPY go.sum ./
RUN go mod download
COPY *.go ./
RUN go build -o go-aku

ENTRYPOINT [ "/app/go-aku" ]