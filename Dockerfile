# go.mod replaces voxlock-go with ../voxlock-go until it is tagged. TAGS=voxlock
# needs it as --build-context voxlock=../voxlock-go; without that the
# wildcards match nothing and stub modules let go mod download resolve the
# replace.
FROM scratch AS voxlock

FROM golang:1.26-alpine AS builder

WORKDIR /src
COPY --from=voxlock go.mo[d] go.su[m] /voxlock-go/
COPY --from=voxlock cud[a]/go.mod cud[a]/go.sum /voxlock-go/cuda/
RUN [ -f /voxlock-go/go.mod ] || { mkdir -p /voxlock-go/cuda \
	&& echo 'module github.com/VoiceBlender/voxlock-go' > /voxlock-go/go.mod \
	&& echo 'module github.com/VoiceBlender/voxlock-go/cuda' > /voxlock-go/cuda/go.mod; }
COPY go.mod go.sum ./
RUN go mod download
COPY --from=voxlock *.go *.s /voxlock-go/
COPY --from=voxlock interna[l] /voxlock-go/internal/
COPY --from=voxlock cud[a]/*.go cud[a]/*.ptx /voxlock-go/cuda/
COPY . .
#RUN CGO_ENABLED=0 go build -tags pprof -o /voiceblender ./cmd/voiceblender
ARG TAGS=""
RUN CGO_ENABLED=0 go build -tags "$TAGS" -o /voiceblender ./cmd/voiceblender

FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata
COPY --from=builder /voiceblender /usr/local/bin/voiceblender
COPY --from=voxlock LICENS[E] NOTIC[E] /usr/share/licenses/voxlock-go/

RUN mkdir -p /tmp/recordings /tmp/tts_cache

EXPOSE 8080/tcp
EXPOSE 5060/udp
EXPOSE 10000-20000/udp

ENTRYPOINT ["voiceblender"]
