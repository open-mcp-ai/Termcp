# Registry ownership marker — must match the server name in server.json:
# io.github.open-mcp-ai/termcp
FROM golang:1.27-alpine AS build
ARG GOPROXY=https://proxy.golang.org,direct
ARG VERSION=dev
ENV GOPROXY=${GOPROXY}
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=$VERSION" -o /out/termcp .

FROM alpine
LABEL io.modelcontextprotocol.server.name="io.github.open-mcp-ai/termcp"

# Dedicated non-root user with a fixed uid/gid so bind-mount owners map
# predictably; termcp resolves its data dir to ~/.termcp by default, so the
# whole state (sessions, SSH configs, message history) lives in the HOME volume.
RUN addgroup -S -g 1000 termcp \
    && adduser -S -u 1000 -G termcp -h /home/termcp termcp

COPY --from=build /out/termcp /usr/local/bin/termcp

# The image is a distribution of the software, so it carries the notices that a
# single binary cannot: the project licence and the vendored terminal's MIT text.
# They are NOT embedded in the binary (see docs/xterm.md).
COPY --from=build /src/LICENSE /src/THIRD_PARTY_NOTICES.md /usr/share/doc/termcp/

USER termcp
ENV HOME=/home/termcp
WORKDIR /home/termcp
VOLUME /home/termcp

EXPOSE 18765

# A default command, because `docker run <image>` used to start `/bin/sh` and
# exit: registry scanners and one-line trials do not read the docs that explain
# that the binary has to be named. The bind is 0.0.0.0 since a published port
# cannot reach the container's loopback, and termcp refuses a non-loopback bind
# without a credential — so TERMCP_AUTH_TOKEN is part of the contract, not an
# option:
#
#   docker run -d -p 18765:18765 -e TERMCP_AUTH_TOKEN=... ghcr.io/open-mcp-ai/termcp:latest
#
# There is still no ENTRYPOINT: a command of your own on the line (`... <image>
# termcp status`) replaces this CMD outright, which is what every documented
# recipe already relies on.
CMD ["termcp", "--host", "0.0.0.0", "--port", "18765"]
