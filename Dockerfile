FROM golang:alpine

WORKDIR /go-aku

RUN apk --update add build-base imagemagick-dev ffmpeg && \
  rm -rf /var/cache/apk/*

COPY go.mod .
COPY go.sum .
RUN go mod download

COPY *.go .
COPY config.toml .

RUN go build -o main .

EXPOSE 8050
CMD ["/go-aku/main"]