FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod *.go ./
RUN CGO_ENABLED=0 go build -trimpath -o /proxy .

FROM alpine:3.23
RUN apk add --no-cache ca-certificates && adduser -D -u 1000 app
WORKDIR /app
COPY --from=build /proxy /app/proxy
USER app
ENV LISTEN_ADDR=0.0.0.0:8789
ENTRYPOINT ["/app/proxy"]
