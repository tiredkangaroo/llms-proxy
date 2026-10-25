# syntax=docker/dockerfile:1

FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY *.go ./
COPY web ./web
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/completions-api .

FROM alpine:3.20
RUN adduser -D -H app
USER app
WORKDIR /app
COPY --from=build /out/completions-api /app/completions-api
EXPOSE 8080
ENV PORT=8080
ENTRYPOINT ["/app/completions-api"]
