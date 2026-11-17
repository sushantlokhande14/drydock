import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// In development the UI runs on Vite's server and the API on drydock's.
export default defineConfig({
  plugins: [react()],
  server: {
    proxy: {
      "/api": { target: "http://localhost:8080", changeOrigin: true },
    },
  },
  build: { outDir: "dist", emptyOutDir: true, sourcemap: false },
});
