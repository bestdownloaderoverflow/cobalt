module.exports = {
  apps: [{
    name: "cobalt-api",
    script: "pnpm",
    args: "start",
    env: {
      API_PORT: 3241,
      API_URL: "https://tiktok-c.snaptik.fit",
    }
  }]
}