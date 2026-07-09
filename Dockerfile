# --- build ---
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod ./
COPY . .
ARG VERSION=docker
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/middleware ./cmd/middleware

# --- runtime (static, minimal) ---
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/middleware /middleware
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/middleware"]
