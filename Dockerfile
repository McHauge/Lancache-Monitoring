# syntax=docker/dockerfile:1.7

FROM golang:1.25-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath -ldflags="-s -w" -o /out/lancache-monitor .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/lancache-monitor /lancache-monitor
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/lancache-monitor"]
