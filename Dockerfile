# syntax=docker/dockerfile:1.7

FROM golang:1.22-alpine AS builder
WORKDIR /src

RUN apk add --no-cache ca-certificates git

COPY go.mod ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o /out/gmail-worker .

FROM gcr.io/distroless/static-debian12:nonroot
WORKDIR /app

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/gmail-worker /app/gmail-worker

ENV PORT=8080
EXPOSE 8080

ENTRYPOINT ["/app/gmail-worker"]
