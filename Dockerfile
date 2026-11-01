# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 GOFLAGS=-trimpath go build -ldflags='-s -w' -o /out/tranp ./cmd/tranp

# Runtime: the binary only. No shell, no package manager, no outbound calls.
FROM scratch
COPY --from=build /out/tranp /tranp
USER 65532:65532
ENV TRANP_ADDR=0.0.0.0:8080
EXPOSE 8080
ENTRYPOINT ["/tranp"]
