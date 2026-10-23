/* Install commands, one entry per tab. `ai` is null on purpose: that tab
   shows copy, not a shell line, and its wording lives in the catalog. */

// Commands Dictionary
const commands = {
  // A sentence for the agent to read, not a shell line: its wording lives in
  // the catalog, so a language change renders it again.
  ai: null,
  claude: "claude mcp add termcp -- npx -y @open-mcp-ai/termcp daemon stdio",
  codex: "codex mcp add termcp -- npx -y @open-mcp-ai/termcp daemon stdio",
  gemini: "gemini mcp add termcp npx -- -y @open-mcp-ai/termcp daemon stdio",
  docker: "docker run -d --name termcp -p 127.0.0.1:18765:18765 -v termcp-data:/home/termcp ghcr.io/open-mcp-ai/termcp:latest termcp --no-internal --host 0.0.0.0 --port 18765 --disable-auth",
  npm: "npx -y @open-mcp-ai/termcp daemon stdio",
  /* One static file per platform; the release assets are named exactly
     termcp-<os>-<arch> (plus .exe on Windows). {{target}} is filled from what
     the browser will tell us about this machine — see detectHost. */
  binary: "curl -fL -o termcp https://github.com/open-mcp-ai/termcp/releases/latest/download/termcp-{{target}}\nchmod +x ./termcp",
  go: "go install github.com/open-mcp-ai/termcp@latest"
};
