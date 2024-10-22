# Stage 1: Build the Go app
FROM golang:1.20-alpine AS builder

WORKDIR /app

COPY . .

# Build the Go application for linux/amd64
RUN GOOS=linux GOARCH=amd64 go build -o my-go-app .

# Stage 2: Create the final image
FROM alpine:latest

RUN apk add --no-cache tzdata
RUN apk add --no-cache curl
RUN curl -sSL https://github.com/argoproj/argo-cd/releases/download/v2.7.9/argocd-linux-amd64 -o /usr/local/bin/argocd
RUN chmod +x /usr/local/bin/argocd

WORKDIR /app

COPY --from=builder /app/ .

EXPOSE 8080

ENTRYPOINT ["./my-go-app"]