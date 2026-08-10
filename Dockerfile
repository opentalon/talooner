# Built and pushed by .github/workflows/release.yml, never by the runner —
# action.yml points at a published digest so a review does not wait on a Go
# compile.
#
# Base images are pinned to explicit versions. Replace with @sha256 digests
# once the first image is published and the digests are known.
FROM golang:1.25.0-alpine3.22 AS build

WORKDIR /src

# No third-party dependencies yet, so there is nothing to warm a layer with
# beyond go.mod itself.
COPY go.mod ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 go build \
      -ldflags "-s -w -X github.com/opentalon/talooner/internal/version.Version=${VERSION}" \
      -o /out/talooner-action ./cmd/talooner-action

# Static, no shell, non-root. The action only makes HTTPS calls; it needs CA
# certificates and nothing else.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/talooner-action /talooner-action

ENTRYPOINT ["/talooner-action"]
