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
# certificates and nothing else. Used by GitHub Actions, which execs the
# image's ENTRYPOINT directly — no shell required.
FROM gcr.io/distroless/static-debian12:nonroot AS github

COPY --from=build /out/talooner-action /talooner-action

ENTRYPOINT ["/talooner-action"]

# GitLab CI's Docker executor runs every job's `script:` through a shell
# inside the container, regardless of the image's own ENTRYPOINT — the
# `github` target above has no shell and can't run there. `debug-nonroot`
# is the same distroless base with busybox (sh + coreutils) added, still
# non-root, still nothing beyond CA certs otherwise.
FROM gcr.io/distroless/static-debian12:debug-nonroot AS gitlab

COPY --from=build /out/talooner-action /talooner-action

ENTRYPOINT ["/talooner-action"]
