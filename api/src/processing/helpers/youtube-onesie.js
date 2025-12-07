import { base64ToU8 } from 'googlevideo/utils';
import { OnesieHeader, OnesieHeaderType, OnesieInnertubeRequest, OnesieInnertubeResponse, OnesieProxyStatus, OnesieRequest, UMPPartId } from 'googlevideo/protos';
import { CompositeBuffer, UmpReader } from 'googlevideo/ump';
import { Constants, YT } from 'youtubei.js';

// huge thanks to https://github.com/LuanRT/googlevideo/blob/main/examples/onesie-request/main.ts
// for basically all of this <3

let _tvClientConfig = {
	nextRefresh: 0,
	data: null
};

const getOnesieHotConfig = async (force) => {
	if (!force && _tvClientConfig.nextRefresh > +new Date()) {
		return _tvClientConfig.data;
	}

	const tvConfig = await fetch("https://www.youtube.com/tv_config?action_get_config=true&client=lb4&theme=cl")
		.then(r => r.text())
		.then(text => JSON.parse(text.split(")]}'")[1]));

	const { onesieHotConfig } = tvConfig.webPlayerContextConfig.WEB_PLAYER_CONTEXT_CONFIG_ID_LIVING_ROOM_WATCH;
	const data = {
		baseUrl: onesieHotConfig.baseUrl,
		clientKey: base64ToU8(onesieHotConfig.clientKey),
		encryptedClientKey: base64ToU8(onesieHotConfig.encryptedClientKey),
		onesieUstreamerConfig: base64ToU8(onesieHotConfig.onesieUstreamerConfig),
	};

	_tvClientConfig.nextRefresh = +new Date() + onesieHotConfig.keyExpiresInSeconds - 30;
	_tvClientConfig.data = data;

	return data;
};

const encodeOnesieRequest = async (payload, clientContext) => {
	const hotConfig = await getOnesieHotConfig();

	const innertubeRequest = OnesieInnertubeRequest.encode({
		url: 'https://youtubei.googleapis.com/youtubei/v1/player?key=AIzaSyDCU8hByM-4DrUqRUYnGn-3llEO78bcxq8',
		headers: Object.entries({
			"Content-Type": "application/json",
			"User-Agent": clientContext.userAgent,
			"X-Goog-Visitor-Id": clientContext.visitorData,
		}).map(([k, v]) => ({ name: k, value: v })),
		body: JSON.stringify(payload),
		proxiedByTrustedBandaid: true,
		skipResponseEncryption: true
	}).finish();

	const encryptedRequest = await encryptRequest(hotConfig.clientKey, innertubeRequest);

	const onesieRequest = OnesieRequest.encode({
		urls: [],
		innertubeRequest: {
			encryptedOnesieInnertubeRequest: encryptedRequest.encrypted,
			hmac: encryptedRequest.hmac,
			iv: encryptedRequest.iv,
			encryptedClientKey: hotConfig.encryptedClientKey,
			serializeResponseAsJson: true,
			useJsonformatterToParsePlayerResponse: false,
		},
		streamerContext: {
			sabrContexts: [],
			unsentSabrContexts: [],
			poToken: undefined,
			playbackCookie: undefined,
			clientInfo: {
				clientName: Constants.CLIENT_NAME_IDS[clientContext.clientName],
				clientVersion: clientContext.clientVersion,
			},
		},
		bufferedRanges: [],
		onesieUstreamerConfig: hotConfig.onesieUstreamerConfig,
	}).finish();

	return {
		onesieRequest,
		baseUrl: hotConfig.baseUrl
	};
};

const readVideoInfoFromUmp = (buffer) => {
	const ump = new UmpReader(new CompositeBuffer([new Uint8Array(buffer)]));

	let playerResponseUmp;
	ump.read(part => {
		if (part.type == UMPPartId.ONESIE_HEADER) {
			const header = OnesieHeader.decode(part.data.chunks[0]);
			if (header.type == OnesieHeaderType.ONESIE_PLAYER_RESPONSE) {
				playerResponseUmp = { header };
			}

		} else if (part.type == UMPPartId.ONESIE_DATA) {
			if (playerResponseUmp && !playerResponseUmp.data) {
				playerResponseUmp.data = OnesieInnertubeResponse.decode(part.data.chunks[0]);
			}
		};
	});

	if (!playerResponseUmp?.data) return null;
	if (playerResponseUmp.data.onesieProxyStatus !== OnesieProxyStatus.OK) {
		throw new Error("Onesie proxy status was not ok:", playerResponseUmp.onesieProxyStatus);
	}

	const asText = new TextDecoder().decode(playerResponseUmp.data.body);
	const asJson = JSON.parse(asText);

	return new YT.VideoInfo([{
		data: asJson,
		status_code: playerResponseUmp.httpStatus,
		success: true,
	}]);
}

const encodeVideoId = (videoId) => {
	return Array.from(base64ToU8(videoId))
		.map(byte => byte.toString(16).padStart(2, '0'))
		.join('');
};


// todo: completely honest: this entire function is entirely copy-pasted
// from https://github.com/LuanRT/googlevideo/blob/main/examples/onesie-request/utils.ts
const encryptRequest = async (clientKey, data) => {
	if (clientKey.length !== 32)
		throw new Error('Invalid client key length');

	const aesKeyData = clientKey.slice(0, 16);
	const hmacKeyData = clientKey.slice(16, 32);

	const iv = crypto.getRandomValues(new Uint8Array(16));

	const aesKey = await crypto.subtle.importKey(
		'raw',
		aesKeyData,
		{ name: 'AES-CTR', length: 128 },
		false,
		['encrypt']
	);

	const encrypted = new Uint8Array(await crypto.subtle.encrypt(
		{ name: 'AES-CTR', counter: iv, length: 128 },
		aesKey,
		data
	));

	const hmacKey = await crypto.subtle.importKey(
		'raw',
		hmacKeyData,
		{ name: 'HMAC', hash: { name: 'SHA-256' } },
		false,
		['sign']
	);

	const hmac = new Uint8Array(await crypto.subtle.sign(
		'HMAC',
		hmacKey,
		new Uint8Array([...encrypted, ...iv])
	));

	return { encrypted, hmac, iv };
}

// todo: remove this once possible
const adjustContext = (ctx, client) => {
	const clonedContext = structuredClone(ctx);
	if (client !== "WEB") delete clonedContext.client.configInfo;

	clonedContext.client.clientName = Constants.CLIENTS[client].NAME;
	clonedContext.client.clientVersion = Constants.CLIENTS[client].VERSION;

	if (client == "WEB_EMBEDDED") {
		clonedContext.client.clientScreen = "EMBED";
		clonedContext.thirdParty = { embedUrl: "https://www.google.com/" };
	}

	return clonedContext;
}

export const getBasicInfo = async (yt, videoId, client) => {
	// todo: figure out how to best adjust the payload's
	// context; the function in yt.js responsible for this
	// is private:
	// https://github.com/LuanRT/YouTube.js/blob/769721c193f2073522a9d35708a07dc8b493f1c7/src/utils/HTTPClient.ts#L202
	const ctx = adjustContext(yt.session.context, client);

	const payload = {
		context: ctx,
		playbackContext: {
			contentPlaybackContext: {
				vis: 0,
				splay: false,
				lactMilliseconds: '-1',
				signatureTimestamp: yt.session.player?.signature_timestamp,
			}
		},
		videoId
	};

	const { onesieRequest, baseUrl } = await encodeOnesieRequest(payload, ctx.client);
	const encodedVideoId = encodeVideoId(videoId);

	// get a cdn url: for some reason this can
	// just 404 for no reason at all
	let gvUrl;
	for (let i = 0; i < 10; i++) {
		const redirectorResponse = await yt.session.http.fetch_function(`https://redirector.googlevideo.com${baseUrl}&id=${encodedVideoId}&cmo:sensitive_content=yes&opr=1&osts=0&por=1&rn=0`, {
			redirect: 'manual'
		});
		gvUrl = redirectorResponse.headers.get("location");

		if (gvUrl) break;
	}

	if (!gvUrl) {
		throw new Error("Unable to get redirector url");
	}

	const initResponse = await yt.session.http.fetch_function(gvUrl, {
		method: "POST",
		headers: {
			"accept": "*/*",
			"content-type": "application/octet-stream"
		},
		referrer: "https://www.youtube.com/",
		body: onesieRequest
	});

	const initResponseBuffer = await initResponse.arrayBuffer();
	const videoInfo = readVideoInfoFromUmp(initResponseBuffer);

	return videoInfo;
};