FROM golang:1.21-alpine

WORKDIR /app

COPY go.* ./
RUN go mod download

COPY main.go main.go

RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o=producer


FROM scratch AS runner

COPY --from=builder /app/producer /producer

ENV REDIS_ADDR=redis-master:6379
ENV REDIS_QUEUE=padok

ENTRYPOINT ["/producer"]
CMD []