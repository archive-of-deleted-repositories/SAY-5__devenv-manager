import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { api, ApiClientError } from "../lib/api";

describe("api client", () => {
  beforeEach(() => {
    globalThis.fetch = vi.fn();
  });
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("lists images on success", async () => {
    (globalThis.fetch as unknown as ReturnType<typeof vi.fn>).mockResolvedValue(
      {
        ok: true,
        status: 200,
        json: async () => ({ images: ["alpine:3.20", "ubuntu:24.04"] }),
      },
    );
    const r = await api.images();
    expect(r.images).toContain("alpine:3.20");
  });

  it("throws ApiClientError on non-2xx", async () => {
    (globalThis.fetch as unknown as ReturnType<typeof vi.fn>).mockResolvedValue(
      {
        ok: false,
        status: 400,
        json: async () => ({
          code: "image_not_allowed",
          message: 'image "x" not in allowlist',
          retryable: false,
        }),
      },
    );
    await expect(api.createSession({ image: "x" })).rejects.toBeInstanceOf(
      ApiClientError,
    );
  });

  it("returns undefined on 204", async () => {
    (globalThis.fetch as unknown as ReturnType<typeof vi.fn>).mockResolvedValue(
      {
        ok: true,
        status: 204,
      },
    );
    const r = await api.deleteSession("s1");
    expect(r).toBeUndefined();
  });
});
