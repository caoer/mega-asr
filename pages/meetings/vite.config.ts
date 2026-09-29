import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";

// The app reads its data live from the page's own registry (/d/, /f/) and
// publishes as a file set: relative asset paths serve under /a/<slug>/.
export default defineConfig({
  base: "./",
  publicDir: false,
  plugins: [react()],
});
