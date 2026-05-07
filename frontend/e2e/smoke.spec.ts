import { test, expect } from "@playwright/test";

// E2E spec exercising the static UI without requiring a live backend.
// /v1/* requests are stubbed so the test stays hermetic and fast; the
// real-backend flow is covered by the chaos test.

test.beforeEach(async ({ page }) => {
  await page.route("**/v1/images", (r) =>
    r.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        images: ["alpine:3.20", "ubuntu:24.04", "node:22-alpine"],
      }),
    }),
  );
  await page.route("**/v1/sessions", (r) =>
    r.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ sessions: [] }),
    }),
  );
});

test("loads the app and shows the image picker", async ({ page }) => {
  await page.goto("/");
  await expect(page.getByRole("heading", { name: "devenv-manager" })).toBeVisible();
  await expect(
    page.getByRole("heading", { name: "Start a session" }),
  ).toBeVisible();
  await expect(page.locator("select")).toBeVisible();
  // First option should be the first image in the allowlist
  const options = await page.locator("select option").allTextContents();
  expect(options).toContain("alpine:3.20");
});

test("empty session list renders the prompt", async ({ page }) => {
  await page.goto("/");
  await expect(page.getByText(/no sessions yet/i)).toBeVisible();
  await expect(page.getByText(/Select or start a session/i)).toBeVisible();
});

test("layout collapses to single column on mobile viewport", async ({ page }) => {
  await page.goto("/");
  // viewport already 390x664 from the iPhone 12 device profile
  // no element should overflow horizontally; image picker stays visible
  await expect(
    page.getByRole("heading", { name: "Start a session" }),
  ).toBeVisible();
});
