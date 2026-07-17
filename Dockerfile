FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -extldflags '-static'" -trimpath -o sub-preprocessor main.go

FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /

COPY --from=builder /app/sub-preprocessor /sub-preprocessor

EXPOSE 8080
EXPOSE 9090

USER nonroot:nonroot

ENTRYPOINT ["/sub-preprocessor"]
