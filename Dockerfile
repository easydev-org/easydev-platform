# Build the platform binary. The easyvcs engine comes from the module proxy
# (github.com/easylab-platform/easyvcs), so this build needs no sibling checkout.
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/easydev-platform ./cmd/easydev-platform

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/easydev-platform /usr/bin/easydev-platform
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/bin/easydev-platform"]
