import { QURLClient } from "@layervai/qurl";

function json(body, status, headers = {}) {
	return Response.json(body, {
		status,
		headers: { "Cache-Control": "no-store", ...headers },
	});
}

export default async function handler(request) {
	if (request.method !== "POST")
		return json({ error: "Use POST" }, 405, { Allow: "POST" });
	const apiKey = Netlify.env.get("QURL_API_KEY");
	const target = Netlify.env.get("QURL_TARGET_URL");
	const token = Netlify.env.get("MINT_TOKEN");
	if (!apiKey || !target || !token || token.length < 32) {
		return json({ error: "Mint endpoint is not configured" }, 503);
	}
	// Compare every character so a matching prefix does not return early.
	const expected = `Bearer ${token}`;
	const supplied = request.headers.get("Authorization") ?? "";
	let mismatch = expected.length ^ supplied.length;
	for (let i = 0; i < expected.length; i++) {
		mismatch |= expected.charCodeAt(i) ^ (supplied.charCodeAt(i) || 0);
	}
	if (mismatch !== 0)
		return json({ error: "Unauthorized" }, 401, {
			"WWW-Authenticate": "Bearer",
		});
	try {
		const url = new URL(target);
		if (url.protocol !== "https:" || url.username || url.password) {
			return json({ error: "Mint endpoint is not configured" }, 503);
		}
	} catch {
		return json({ error: "Mint endpoint is not configured" }, 503);
	}
	try {
		const client = new QURLClient({
			apiKey,
			timeout: 10_000,
			maxRetries: 0,
		});
		const { portal } = await client.createPortalForUrl(target, {
			validFor: "60s",
		});
		return json({ link: portal.link, expires_at: portal.expiresAt }, 201);
	} catch {
		// API errors can contain resource details; do not return or log them.
		return json({ error: "Could not mint a link" }, 502);
	}
}

export const config = { path: "/api/create-qurl" };
