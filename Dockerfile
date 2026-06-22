FROM golang:1.26-alpine AS builder
WORKDIR /build
COPY go.mod go.sum ./
ENV GOPROXY=https://goproxy.cn,direct
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o general-exporter .

FROM alpine:3.19
RUN apk add --no-cache docker-cli curl jq
WORKDIR /app
COPY --from=builder /build/general-exporter .
COPY config.yaml .
COPY scripts/ ./scripts/
RUN chmod +x general-exporter && find scripts/ -type f -exec chmod +x {} \;
ARG EXPORTER_PORT=8081
EXPOSE ${EXPORTER_PORT}
ENTRYPOINT ["./general-exporter"]
