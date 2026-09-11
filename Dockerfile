FROM docker.m.daocloud.io/library/golang:1.25-alpine AS builder

WORKDIR /builder
COPY upstream/ ./upstream/
COPY bot.go ./upstream/bot.go
ENV GOPROXY=https://goproxy.cn,direct
RUN cd upstream && go mod download
RUN cd upstream && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o ../ocr ./cmd/opencodereview/
RUN cd upstream && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o ../ocr-bot-server ./bot.go

FROM docker.m.daocloud.io/library/alpine:3.20

# Create directories for persistent storage
RUN mkdir -p /data/ocr-reviews /data/ocr-home/.opencodereview/sessions && \
    chmod 755 /data/ocr-reviews /data/ocr-home

WORKDIR /root/
RUN apk add --no-cache git
COPY --from=builder /builder/ocr /usr/local/bin/ocr
COPY --from=builder /builder/ocr-bot-server /usr/local/bin/ocr-bot-server
RUN chmod +x /usr/local/bin/ocr /usr/local/bin/ocr-bot-server

# Volumes for persistent storage (mount at runtime)
VOLUME ["/data/ocr-reviews", "/data/ocr-home/.opencodereview/sessions"]

CMD ["ocr-bot-server"]
