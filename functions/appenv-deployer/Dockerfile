FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /function .

FROM gcr.io/distroless/static-debian11
COPY --from=builder /function /function
EXPOSE 9443
ENTRYPOINT ["/function"]
