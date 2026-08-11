import { defineConfig, loadEnv } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, process.cwd(), "");
  const apiTarget = env.VITE_DEV_API_TARGET || "http://127.0.0.1:8080";

  return {
    plugins: [react(), tailwindcss()],
    server: {
      port: Number(env.FRONTEND_PORT || 5173),
      proxy: {
        "/api": {
          target: apiTarget,
          changeOrigin: true,
        },
        "/ws": {
          target: apiTarget.replace(/^http/, "ws"),
          ws: true,
        },
      },
    },
    build: {
      target: "es2022",
    },
  };
});
