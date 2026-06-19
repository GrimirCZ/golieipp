FROM golang:1.26-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/golieipp ./cmd/golieipp

FROM alpine:3.23

RUN addgroup -S golieipp \
	&& adduser -S -D -H -h /opt/golieipp -s /sbin/nologin -G golieipp golieipp \
	&& apk add --no-cache ca-certificates \
	&& install -d -o golieipp -g golieipp /opt/golieipp /data

COPY --from=build /out/golieipp /opt/golieipp/golieipp

USER golieipp:golieipp
WORKDIR /data
EXPOSE 8631

ENTRYPOINT ["/opt/golieipp/golieipp"]
CMD ["-config", "/opt/golieipp/config.yaml"]
