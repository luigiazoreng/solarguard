FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /solarguard ./cmd/solarguard

FROM alpine:3.22
RUN apk add --no-cache ca-certificates sqlite \
 && adduser -S -D -H -u 10001 solarguard \
 && install -d -o solarguard -m 700 /data
COPY --from=build /solarguard /usr/local/bin/solarguard
USER solarguard
ENV SOLARGUARD_DB=/data/solarguard.db SOLARGUARD_ADDR=0.0.0.0:8080
EXPOSE 8080
CMD ["solarguard", "serve"]
