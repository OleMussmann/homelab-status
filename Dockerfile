# Built by incus-compose from a git context in ~/code/IncusOS/Monitoring.
# Both bases are pinned by digest; bump the tag and digest together.

FROM docker.io/library/golang:1.25-alpine@sha256:1ae0735f00daffa3aaf1363a5184c0d2dc55c78e3db4ec70241cdac97bf84b59 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/homelab-status ./cmd/homelab-status

# Static binary only: distroless has CA certs and tzdata but no shell, so the
# compose healthcheck runs `homelab-status -healthcheck`.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/homelab-status /usr/local/bin/homelab-status
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/homelab-status"]
CMD ["-config", "/config/config.toml"]
