async function unshortenUrl(shortId) {
	const response = await fetch(`https://maps.app.goo.gl/${shortId}`, {
		redirect: 'manual'
	});
	return response.headers.get("Location");
}

const PartType = {
	'Array': 'm',
	'Boolean': 'b',
	'Float': 'f',
	'Double': 'd',
	'Int': 'i',
	'Uint': 'u',
	'EnumValue': 'e',
	'String': 's'
};

function decodeData(encoded) {
	const parts = decodeURIComponent(encoded).split('!').filter(part => part.length != 0);
	const parsed = {};

	for (let i = 0; i < parts.length; i++) {
		const [ id, type ] = parts[i];
		const partData = parts[i].substring(2);

		switch (type) {
			case PartType.Array:
				const len = parseInt(partData);
				parsed[id] = decodeData(parts.slice(i + 1, len).join("!"));
				i += len;
				break;
			case PartType.Boolean:
				parsed[id] = Boolean(partData);
				break;
			case PartType.Double:
			case PartType.EnumValue:
			case PartType.Float:
			case PartType.Int:
			case PartType.Uint:
				parsed[id] = Number(partData);
				break;
			case PartType.String:
				parsed[id] = partData;
				break;
			default:
				console.warn("unknown type");
				break;
		}
	}

	return parsed;
}

async function getPhotoMeta(mediaId) {
	// todo: build this url ourselves and find out what the parameters do
	const metaUrl = `https://www.google.com/maps/photometa/v1?pb=!1m4!1smaps_sv.tactile!11m2!2m1!1b1!2m2!1sde!2sat!3m5!1m2!1e10!2s${mediaId}!2m1!5s0x476d073f4daba163:0xfce7699ce9a8b1f4!4m61!1e1!1e2!1e3!1e4!1e5!1e6!1e8!1e12!1e17!2m1!1e1!4m1!1i48!5m1!1e1!5m1!1e2!6m1!1e1!6m1!1e2!9m36!1m3!1e2!2b1!3e2!1m3!1e2!2b0!3e3!1m3!1e3!2b1!3e2!1m3!1e3!2b0!3e3!1m3!1e8!2b0!3e3!1m3!1e1!2b0!3e3!1m3!1e4!2b0!3e3!1m3!1e10!2b1!3e2!1m3!1e10!2b0!3e3!11m2!3m1!4b1`;
	const metaData = await fetch(metaUrl)
		.then(r => r.text())
		.then(text => JSON.parse(text.split("\n")[1]));

	return metaData;
}

const MEDIA_TYPE_PHOTO = 3;
const MEDIA_TYPE_VIDEO = 4;

export default async function(o) {
	const fullUrl = await unshortenUrl(o.id);

	const encodedData = new URL(fullUrl).pathname.split("data=")[1];
	const data = decodeData(encodedData);

	const mediaId = data[3][3][1];

	if (!mediaId) {
		return { error: "fetch.fail" };
	}

	const meta = await getPhotoMeta(mediaId);
	const actualMetadata = meta[1][0];

	const [, mediaType] = actualMetadata[2];

	if (mediaType == MEDIA_TYPE_PHOTO) {
		const preview = actualMetadata[17][0][0];
		let finalUrl = preview.split("=")[0] + "=";
		finalUrl += "w0-"; // full resolution
		finalUrl += "ip"; // keep metadata

		return {
			isPhoto: true,
			urls: finalUrl,
			filename: `${mediaId}.jpg`
		};
	} else if (mediaType == MEDIA_TYPE_VIDEO) {
		const mediaInfo = actualMetadata[2].at(-1);
		const [, formats] = mediaInfo;

		const highestResolutionFormat = formats
			.filter(format => !format[3].endsWith("hls") && !format[3].endsWith("dash"))
			.at(-1);
		
		return {
			urls: highestResolutionFormat[3],
			filename: `${mediaId}.mp4`
		};
	}

	return {
		error: "fetch.empty"
	};
}