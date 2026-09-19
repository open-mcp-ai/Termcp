# Registry ownership marker — must match the server name in server.json:
# io.github.open-mcp-ai/termcp
FROM golang:1.25-alpine AS build
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=${GOPROXY}
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/termcp .

FROM alpine
LABEL io.modelcontextprotocol.server.name="io.github.open-mcp-ai/termcp"

# Dedicated non-root user with a fixed uid/gid so bind-mount owners map
# predictably; termcp resolves its data dir to ~/.termcp by default, so the
# whole state (sessions, SSH configs, history) lives in the HOME volume.
RUN addgroup -S -g 1000 termcp \
    && adduser -S -u 1000 -G termcp -h /home/termcp termcp

COPY --from=build /out/termcp /usr/local/bin/termcp

USER termcp
ENV HOME=/home/termcp
WORKDIR /home/termcp
VOLUME /home/termcp
EXPOSE 18765

ENTRYPOINT ["termcp", "--host", "0.0.0.0", "--port", "18765"]
