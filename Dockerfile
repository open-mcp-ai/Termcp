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
COPY --from=build /out/termcp /usr/local/bin/termcp
EXPOSE 18765
VOLUME /data
ENTRYPOINT ["termcp", "--host", "0.0.0.0", "--port", "18765", "--data-dir", "/data"]
