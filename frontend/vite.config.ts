import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";
import wails from "@wailsio/runtime/plugins/vite";

// https://vitejs.dev/config/
export default defineConfig({
  build: {
    rollupOptions: {
      // Two entry pages: the main application and the Stage 5A
      // first-launch welcome page (served by the welcome window
      // before any formal data exists).
      input: {
        main: "./index.html",
        welcome: "./welcome.html",
      },
    },
  },
  server: {
    host: "127.0.0.1",
    port: Number(process.env.WAILS_VITE_PORT) || 9245,
    strictPort: true,
  },
  plugins: [react(), tailwindcss(), wails("./bindings")],
});
