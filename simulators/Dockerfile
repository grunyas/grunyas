FROM golang:1.25-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o grunyas ./cmd

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=builder /app/grunyas .
ENV GRUNYAS_SERVER_PPROF_ADDR=0.0.0.0:6060
CMD ["./grunyas", "-no-console"]
