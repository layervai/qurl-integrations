import { describe, it, expect, vi } from "vitest";
import { QURLClient } from "./client.js";
import { QURLError } from "./errors.js";

function mockFetch(response: {
  status: number;
  body?: unknown;
  headers?: Record<string, string>;
}): typeof globalThis.fetch {
  return vi.fn().mockResolvedValue({
    ok: response.status >= 200 && response.status < 400,
    status: response.status,
    statusText: response.status === 200 ? "OK" : "Error",
    headers: new Headers(response.headers ?? {}),
    json: () => Promise.resolve(response.body),
    text: () => Promise.resolve(JSON.stringify(response.body)),
  } satisfies Partial<Response> as Response);
}

function createClient(fetchFn: typeof globalThis.fetch): QURLClient {
  return new QURLClient({
    apiKey: "lv_live_test",
    baseUrl: "https://api.test.layerv.ai",
    fetch: fetchFn,
    maxRetries: 0,
  });
}

describe("QURLClient", () => {
  it("creates a QURL", async () => {
    const fetch = mockFetch({
      status: 201,
      body: {
        data: {
          resource_id: "r_abc123def45",
          qurl_link: "https://qurl.link/#at_test",
          qurl_site: "https://r_abc123def45.qurl.site",
          expires_at: "2026-03-15T10:00:00Z",
        },
        meta: { request_id: "req_1" },
      },
    });

    const client = createClient(fetch);
    const result = await client.create({
      target_url: "https://example.com",
      expires_in: "24h",
    });

    expect(result.resource_id).toBe("r_abc123def45");
    expect(result.qurl_link).toBe("https://qurl.link/#at_test");
    expect(fetch).toHaveBeenCalledWith(
      "https://api.test.layerv.ai/v1/qurl",
      expect.objectContaining({ method: "POST" }),
    );
  });

  it("gets a QURL", async () => {
    const fetch = mockFetch({
      status: 200,
      body: {
        data: {
          resource_id: "r_abc123def45",
          target_url: "https://example.com",
          status: "active",
          created_at: "2026-03-10T10:00:00Z",
        },
      },
    });

    const client = createClient(fetch);
    const result = await client.get("r_abc123def45");

    expect(result.resource_id).toBe("r_abc123def45");
    expect(result.status).toBe("active");
  });

  it("lists QURLs", async () => {
    const fetch = mockFetch({
      status: 200,
      body: {
        data: [
          {
            resource_id: "r_abc123def45",
            target_url: "https://example.com",
            status: "active",
            created_at: "2026-03-10T10:00:00Z",
          },
        ],
        meta: { has_more: false, page_size: 20 },
      },
    });

    const client = createClient(fetch);
    const result = await client.list({ status: "active", limit: 10 });

    expect(result.qurls).toHaveLength(1);
    expect(result.qurls[0].resource_id).toBe("r_abc123def45");
    expect(result.has_more).toBe(false);
  });

  it("deletes a QURL", async () => {
    const fetch = mockFetch({ status: 204 });
    const client = createClient(fetch);

    await expect(client.delete("r_abc123def45")).resolves.toBeUndefined();
  });

  it("extends a QURL", async () => {
    const fetch = mockFetch({
      status: 200,
      body: {
        data: {
          resource_id: "r_abc123def45",
          target_url: "https://example.com",
          status: "active",
          created_at: "2026-03-10T10:00:00Z",
          expires_at: "2026-03-20T10:00:00Z",
        },
      },
    });

    const client = createClient(fetch);
    const result = await client.extend("r_abc123def45", { extend_by: "7d" });

    expect(result.expires_at).toBe("2026-03-20T10:00:00Z");
  });

  it("resolves a QURL token", async () => {
    const fetch = mockFetch({
      status: 200,
      body: {
        data: {
          target_url: "https://api.example.com/data",
          resource_id: "r_abc123def45",
          access_grant: {
            expires_in: 305,
            granted_at: "2026-03-10T15:30:00Z",
            src_ip: "203.0.113.42",
          },
        },
      },
    });

    const client = createClient(fetch);
    const result = await client.resolve({
      access_token: "at_k8xqp9h2sj9lx7r4a",
    });

    expect(result.target_url).toBe("https://api.example.com/data");
    expect(result.access_grant?.expires_in).toBe(305);
    expect(result.access_grant?.src_ip).toBe("203.0.113.42");
  });

  it("throws QURLError on API errors", async () => {
    const fetch = mockFetch({
      status: 404,
      body: {
        error: {
          type: "https://api.qurl.link/problems/not_found",
          title: "Not Found",
          status: 404,
          detail: "QURL not found",
          code: "not_found",
        },
        meta: { request_id: "req_err" },
      },
    });

    const client = createClient(fetch);

    try {
      await client.get("r_notfound0000");
      expect.fail("Expected QURLError");
    } catch (err) {
      expect(err).toBeInstanceOf(QURLError);
      const qErr = err as QURLError;
      expect(qErr.status).toBe(404);
      expect(qErr.code).toBe("not_found");
      expect(qErr.requestId).toBe("req_err");
    }
  });

  it("sends correct auth header", async () => {
    const fetch = mockFetch({
      status: 200,
      body: {
        data: {
          plan: "growth",
          period_start: "2026-03-01",
          period_end: "2026-04-01",
        },
      },
    });

    const client = createClient(fetch);
    await client.getQuota();

    expect(fetch).toHaveBeenCalledWith(
      expect.any(String),
      expect.objectContaining({
        headers: expect.objectContaining({
          Authorization: "Bearer lv_live_test",
        }),
      }),
    );
  });

  it("mints a link", async () => {
    const fetch = mockFetch({
      status: 200,
      body: {
        data: {
          qurl_link: "https://qurl.link/#at_newtoken",
          expires_at: "2026-03-15T10:00:00Z",
        },
      },
    });

    const client = createClient(fetch);
    const result = await client.mintLink("r_abc123def45");

    expect(result.qurl_link).toBe("https://qurl.link/#at_newtoken");
    expect(fetch).toHaveBeenCalledWith(
      "https://api.test.layerv.ai/v1/qurls/r_abc123def45/mint_link",
      expect.objectContaining({ method: "POST" }),
    );
  });
});
