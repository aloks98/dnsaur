import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

export default defineConfig({
  plugins: [react(), tailwindcss()],
  server: {
    host: "127.0.0.1",
    port: 5280,
    // DNSAUR_API points the dev server at another dnsaur, e.g. one running a
    // branch over a scratch data dir while the usual one keeps its database.
    proxy: { "/api": process.env.DNSAUR_API ?? "http://127.0.0.1:8380" },
  },
  build: { outDir: "dist", emptyOutDir: true },
});
