# syntax=docker/dockerfile:1.7

FROM golang:1.25-alpine AS build
WORKDIR /src

# git is required so `go build` can embed VCS info (vcs.revision / vcs.modified)
# into the binary; we surface that via debug.ReadBuildInfo() at startup.
RUN apk add --no-cache git

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath -ldflags="-s -w" -o /out/lancache-monitor .
RUN mkdir /data-seed

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/lancache-monitor /lancache-monitor
COPY --from=build --chown=65532:65532 /data-seed /data
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/lancache-monitor"]
