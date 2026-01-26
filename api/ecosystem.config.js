module.exports = {
  apps: [{
    name: "cobalt-api",
    script: "./src/cobalt.js",
    env: {
      API_PORT: 3241,
      // Tambahkan variabel lain jika perlu
    }
  }]
}