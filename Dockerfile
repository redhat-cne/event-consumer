FROM golang:1.26 AS builder
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /cloud-event-consumer ./cmd/cloud-event-consumer
FROM registry.access.redhat.com/ubi9/ubi-minimal:latest
COPY --from=builder /cloud-event-consumer /cloud-event-consumer
USER 65532:65532
ENTRYPOINT ["/cloud-event-consumer"]
