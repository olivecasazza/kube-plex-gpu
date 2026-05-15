FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /kube-plex-gpu .

FROM alpine:3.20
COPY --from=builder /kube-plex-gpu /kube-plex-gpu
ENTRYPOINT ["/kube-plex-gpu"]
