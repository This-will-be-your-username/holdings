FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /holdings .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata && adduser -D -u 10001 app && mkdir /data && chown app /data
COPY --from=build /holdings /usr/local/bin/holdings
USER app
ENV ADDR=0.0.0.0:8080 DATA_FILE=/data/fundtrack.json
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["holdings"]
