FROM golang:1.25-alpine AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /voiceblender ./cmd/voiceblender

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /voiceblender /usr/local/bin/voiceblender

RUN mkdir -p /tmp/recordings /tmp/tts_cache

EXPOSE 8080/tcp
EXPOSE 5060/udp
EXPOSE 10000-20000/udp

ENTRYPOINT ["voiceblender"]
