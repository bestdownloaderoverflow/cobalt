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

export const getMinter = async ({ fetch }) => {
	if (!domInitialized) {
		initDOM();
	}

	const challenge = await fetch(buildURL("Create", true), {
		method: "POST",
		headers: {
			"content-type": "application/json+protobuf",
			"x-goog-api-key": GOOG_API_KEY,
			"x-user-agent": "grpc-web-javascript/0.1"
		},
		body: JSON.stringify([ YOUTUBE_KEY ])
	}).then(r => r.json());

	const bgChallenge = BG.Challenge.parseChallengeData(challenge);
	new Function(bgChallenge.interpreterJavascript.privateDoNotAccessOrElseSafeScriptWrappedValue)();

	const bg = await BG.BotGuardClient.create({
		program: bgChallenge.program,
		globalName: bgChallenge.globalName,
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