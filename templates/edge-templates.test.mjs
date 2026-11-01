import assert from "node:assert/strict";
import test from "node:test";

const env = {
	QURL_API_KEY: "test-api-key-not-a-live-credential",
	QURL_TARGET_URL: "https://internal.example.com/admin",
	MINT_TOKEN: "a".repeat(64),
};
const data = {
	resource_id:
		"MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE2cTVv5_3eeYCcLLq5ROYCqcmY50HiKZ9ATglIkPnCji1E_S63UMtXba1moR8-Q6EV7oM6zwwh9_j2CDujzXvLA",
	crid: "ahpviqz46qwcvx56glfatm3p3ooccwfcf2it4sdgjervwdkapykw3o3qdq2a",
	qurl_link: "https://qurl.link/#at_template_fixture",
	expires_at: "2027-01-01T00:01:00Z",
};
const platforms = [
	["cloudflare-worker", "./cloudflare-worker/src/index.js"],
	["netlify-edge", "./netlify-edge/netlify/edge-functions/create-qurl.js"],
	["vercel-edge", "./vercel-edge/api/create-qurl.js"],
];

for (const [name, path] of platforms) {
	test(name, async () => {
		const module = await import(path);
		const savedFetch = globalThis.fetch;
		const savedNetlify = globalThis.Netlify;
		const savedEnv = Object.fromEntries(
			Object.keys(env).map((key) => [key, process.env[key]]),
		);
		let calls = [];
		let upstream = () => Response.json({ data }, { status: 201 });
		globalThis.fetch = async function (url, init) {
			assert.equal(
				this,
				globalThis,
				"native fetch must keep its global receiver",
			);
			calls.push({ url: String(url), init });
			return upstream();
		};
		async function invoke({
			values = env,
			authorization = `Bearer ${env.MINT_TOKEN}`,
			method = "POST",
			body,
		} = {}) {
			for (const key of Object.keys(env)) {
				if (values[key] === undefined) delete process.env[key];
				else process.env[key] = values[key];
			}
			globalThis.Netlify = { env: { get: (key) => values[key] } };
			const request = new Request(
				"https://mint.example.com/api/create-qurl?target=https://evil.example",
				{
					method,
					headers: authorization
						? { Authorization: authorization }
						: {},
					body,
				},
			);
			const response =
				name === "cloudflare-worker"
					? await module.default.fetch(request, values)
					: await module.default(request);
			assert.equal(response.headers.get("Cache-Control"), "no-store");
			return response;
		}
		try {
			assert.equal((await invoke({ method: "GET" })).status, 405);
			assert.equal((await invoke({ authorization: "" })).status, 401);
			for (const token of [
				"b".repeat(64),
				"a".repeat(63),
				"a".repeat(65),
			]) {
				assert.equal(
					(await invoke({ authorization: `Bearer ${token}` })).status,
					401,
				);
			}
			for (const key of Object.keys(env)) {
				assert.equal(
					(await invoke({ values: { ...env, [key]: "" } })).status,
					503,
				);
			}
			assert.equal(
				(await invoke({ values: { ...env, MINT_TOKEN: "short" } }))
					.status,
				503,
			);
			for (const target of [
				"not-a-url",
				"http://internal.example.com",
				"https://user:pass@internal.example.com",
			]) {
				assert.equal(
					(
						await invoke({
							values: { ...env, QURL_TARGET_URL: target },
						})
					).status,
					503,
				);
			}
			assert.equal(
				calls.length,
				0,
				"invalid or unauthorized requests must not contact qURL",
			);
			for (const key of Object.keys(env)) {
				assert.equal(
					(await invoke({ values: { ...env, [key]: undefined } }))
						.status,
					503,
				);
			}
			assert.equal(
				(await invoke({ authorization: env.MINT_TOKEN })).status,
				401,
			);
			const response = await invoke({
				body: JSON.stringify({
					target: "https://evil.example",
					expires_in: "30d",
				}),
			});
			assert.equal(response.status, 201);
			assert.deepEqual(await response.json(), {
				link: data.qurl_link,
				expires_at: new Date(data.expires_at).toISOString(),
			});
			assert.equal(calls.length, 1);
			assert.equal(new URL(calls[0].url).pathname, "/v1/qurls");
			assert.equal(calls[0].init.method, "POST");
			assert.deepEqual(JSON.parse(calls[0].init.body), {
				target_url: env.QURL_TARGET_URL,
				expires_in: "60s",
			});
			assert.equal(
				new Headers(calls[0].init.headers).get("Authorization"),
				`Bearer ${env.QURL_API_KEY}`,
			);
			assert.equal(
				(await invoke({ authorization: `bearer ${env.MINT_TOKEN}` }))
					.status,
				201,
			);
			assert.equal(calls.length, 2);
			calls = [];
			upstream = () =>
				Response.json(
					{ error: { message: "secret upstream detail" } },
					{ status: 503 },
				);
			const failed = await invoke();
			assert.equal(failed.status, 502);
			assert.deepEqual(await failed.json(), {
				error: "Could not mint a link",
			});
			assert.equal(calls.length, 1, "failed mint must not be retried");
			upstream = () => {
				throw new Error("secret transport detail");
			};
			const transportFailure = await invoke();
			assert.equal(transportFailure.status, 502);
			assert.deepEqual(await transportFailure.json(), {
				error: "Could not mint a link",
			});
			upstream = () =>
				Response.json(
					{ data: { ...data, crid: "wrong-resource" } },
					{ status: 201 },
				);
			assert.equal(
				(await invoke()).status,
				502,
				"SDK resource-identity validation must remain active",
			);
			if (name === "netlify-edge")
				assert.deepEqual(module.config, { path: "/api/create-qurl" });
			if (name === "vercel-edge")
				assert.deepEqual(module.config, { runtime: "edge" });
		} finally {
			globalThis.fetch = savedFetch;
			if (savedNetlify === undefined) delete globalThis.Netlify;
			else globalThis.Netlify = savedNetlify;
			for (const [key, value] of Object.entries(savedEnv)) {
				if (value === undefined) delete process.env[key];
				else process.env[key] = value;
			}
		}
	});
}
