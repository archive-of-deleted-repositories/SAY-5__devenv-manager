import { defineConfig, devices } from "@playwright/test";

const PORT = 5173;

export default defineConfig({
  testDir: "./e2e",
  timeout: 30_000,
  fullyParallel: false,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  reporter: process.env.CI ? "github" : "list",
  use: {
    baseURL: `http://localhost:${PORT}`,
    trace: "on-first-retry",
  },
  webServer: {
    command: `pnpm dev --port ${PORT}`,
    port: PORT,
    reuseExistingServer: !process.env.CI,
    timeout: 60_000,
  },
  projects: [
    { name: "chromium", use: { ...devices["Desktop Chrome"] } },
    // Pixel 5 (chromium-based) instead of iPhone 12 (webkit) so CI only
    // needs a single browser install. The viewport-overflow assertions
    // are what matters; they are agnostic to the rendering engine.
    { name: "mobile", use: { ...devices["Pixel 5"] } },
  ],
});
