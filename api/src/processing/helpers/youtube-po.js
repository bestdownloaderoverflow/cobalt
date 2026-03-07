import { BG, buildURL, GOOG_API_KEY, USER_AGENT } from 'bgutils-js';
import { JSDOM } from 'jsdom';

const YOUTUBE_KEY = "O43z0dpjhgX20SCx4KAo";

let domInitialized = false;

// I'm aware that I probably shouldn't be polluting globalThis like this
// but it'll probably work out for now
const initDOM = () => {
	domInitialized = true;

	const dom = new JSDOM('<!DOCTYPE html><html lang="en"><head><title></title></head><body></body></html>', {
		url: 'https://www.youtube.com/',
		referrer: 'https://www.youtube.com/',
		userAgent: USER_AGENT
	});

	Object.assign(globalThis, {
		window: dom.window,
		document: dom.window.document,
		location: dom.window.location,
		origin: dom.window.origin
	});

	if (!Reflect.has(globalThis, 'navigator')) {
		Object.defineProperty(globalThis, 'navigator', { value: dom.window.navigator });
	}
};

const fetchChallengeFromInnertube = async (yt) => {
	const attestChallenge = await yt.getAttestationChallenge("ENGAGEMENT_TYPE_UNBOUND");
	const challengeJsUrl = new URL(attestChallenge.bg_challenge.interpreter_url.private_do_not_access_or_else_trusted_resource_url_wrapped_value, "https://a");

	return {
		code: await fetch(challengeJsUrl.toString()).then(r => r.text()),
		program: attestChallenge.bg_challenge.program,
		globalName: attestChallenge.bg_challenge.global_name,
	};
}

const fetchChallengeFromWaa = async (fetch) => {
	const attestChallenge = await fetch(buildURL("Create", true), {
		method: "POST",
		headers: {
			"content-type": "application/json+protobuf",
			"x-goog-api-key": GOOG_API_KEY,
			"x-user-agent": "grpc-web-javascript/0.1"
		},
		body: JSON.stringify([ YOUTUBE_KEY ])
	}).then(r => r.json());

	const bgChallenge = BG.Challenge.parseChallengeData(attestChallenge);

	return {
		code: bgChallenge.interpreterJavascript.privateDoNotAccessOrElseSafeScriptWrappedValue,
		program: bgChallenge.program,
		globalName: bgChallenge.globalName,
	};
}

export const getMinter = async ({ yt, fetch }) => {
	if (!domInitialized) {
		initDOM();
	}

	// fallback to waa, I had some issues with innertube
	// rate-limiting me here(..?)
	const challenge = await fetchChallengeFromInnertube(yt)
		.catch(() => fetchChallengeFromWaa(fetch));

	new Function(challenge.code)();

	const bg = await BG.BotGuardClient.create({
		program: challenge.program,
		globalName: challenge.globalName,
		globalObj: globalThis,
		fetch,
	});

	const poSignalOutput = [];
	const integrityTokenResponse = await fetch(buildURL('GenerateIT', true), {
		method: 'POST',
		headers: {
			'content-type': 'application/json+protobuf',
			'x-goog-api-key': GOOG_API_KEY,
			'x-user-agent': 'grpc-web-javascript/0.1',
			'user-agent': USER_AGENT
		},
		body: JSON.stringify([ YOUTUBE_KEY, await bg.snapshot({ webPoSignalOutput: poSignalOutput }) ])
	}).then(r => r.json());

	const integrityTokenBasedMinter = await BG.WebPoMinter.create({ integrityToken: integrityTokenResponse[0] }, poSignalOutput);

	const remove = () => bg.shutdown();

	return {
		minter: integrityTokenBasedMinter,
		remove
	};
};