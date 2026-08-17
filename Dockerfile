# Drive — production image. Three stages: build the SPA, build the binary that
# embeds it, ship the binary on a base that can speak TLS.
#
# Deliberately does not use the Makefile or go.work. The Makefile hardcodes a
# Homebrew PATH, and go.work.sum is untracked — a build that resolved the
# workspace would work from a local checkout and fail from a clean one. The
# server module is self-contained (server/go.mod + server/go.sum), so this
# builds it directly.

# --- SPA -------------------------------------------------------------------
# Vite's outDir is ../server/web/dist (web/vite.config.ts), so the build writes
# outside this stage's workdir on purpose: that path is what go:embed reads.
FROM node:26-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build \
 && test -d /src/server/web/dist/assets \
 && test -n "$(ls -A /src/server/web/dist/assets)" \
 && test -s /src/server/web/dist/index.html \
 && ! grep -q 'has not been built yet' /src/server/web/dist/index.html
# server/web/dist holds a committed placeholder index.html so a fresh clone
# compiles; go build accepts it silently, and the result would be a server
# serving "the web app has not been built yet" at the real URL.
#
# What actually keeps the placeholder out of the image is structural, not this
# grep: .dockerignore excludes server/web/dist from the context, so the only
# path into the binary is the COPY --from=web below. The grep is the backstop
# for the day that changes. `test -s` is not decoration either -- `grep -q` on a
# missing file exits 2, which the leading `!` turns into success, so without it
# a build that emitted assets but no entry HTML would pass and ship a server
# that 404s at /.

# --- server ----------------------------------------------------------------
FROM golang:1.26-alpine AS server
WORKDIR /src/server
COPY server/go.mod server/go.sum ./
RUN go mod download
COPY server/ ./
# .dockerignore excludes server/web/dist from the context, so this is the only
# source of the embedded SPA — a stale local build cannot leak into the image.
COPY --from=web /src/server/web/dist ./web/dist
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags='-s -w' -o /out/drive ./cmd/drive

# --- runtime ---------------------------------------------------------------
# Not scratch: every outbound call this server makes is TLS (R2, Postgres,
# Resend), and a scratch image has no root store. Measured, not assumed — the
# same binary on scratch dies at boot with
#   DRIVE_S3_ENDPOINT: unreachable: x509: certificate signed by unknown authority
# from ValidateRuntime's probe. Alpine's base already carries the bundle, so the
# apk line is what guarantees the store rather than merely restating it. The
# test below documents the path the Go runtime reads and would catch a base
# image that moved it -- it cannot catch a missing bundle, since it runs after
# apk in the same chain and apk failing fails the build first.
FROM alpine:3.22
RUN apk add --no-cache ca-certificates \
 && test -f /etc/ssl/certs/ca-certificates.crt \
 && adduser -D -H -u 10001 drive
COPY --from=server /out/drive /usr/local/bin/drive
USER drive
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/drive"]
