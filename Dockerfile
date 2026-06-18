FROM golang:1.26-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o general-exporter .

FROM alpine:3.19
RUN apk add --no-cache docker-cli
WORKDIR /app
COPY --from=builder /build/general-exporter .
COPY config.yaml .
RUN chmod +x general-exporter
EXPOSE 8081
ENTRYPOINT ["./general-exporter"]
