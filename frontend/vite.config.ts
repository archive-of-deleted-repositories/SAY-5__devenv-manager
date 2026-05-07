import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

const BACKEND = process.env.VITE_BACKEND ?? "http://localhost:8080";

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      "/healthz": BACKEND,
      "/v1/sessions": { target: BACKEND, ws: true },
      "/v1/images": BACKEND,
      "/metrics": BACKEND,
    },
  },
});
