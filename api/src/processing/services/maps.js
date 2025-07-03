async function unshortenUrl(shortId) {
	const response = await fetch(`https://maps.app.goo.gl/${shortId}`, {
		redirect: 'manual'
	});
	return response.headers.get("Location");
}

function decodeData(dataString) {
	// shamelessly stole this parsing part from someone - should probably check who it is
	var parts = dataString.split('!').filter(function(s) { return s.length > 0; }),
		root = [],                      // Root elemet
		curr = root,                    // Current array element being appended to
		m_stack = [root,],              // Stack of "m" elements
		m_count = [parts.length,];      // Number of elements to put under each level

	parts.forEach(function(el) {
		var kind = el.substr(1, 1),
			value = el.substr(2);

		// Decrement all the m_counts
		for (var i = 0; i < m_count.length; i++) {
			m_count[i]--;
		}

		if (kind === 'm') {            // Add a new array to capture coming values
			var new_arr = [];
			m_count.push(value);
			curr.push(new_arr);
			m_stack.push(new_arr);
			curr = new_arr;
		}
		else {
			if (kind == 'b') {                                    // Assuming these are boolean
				curr.push(value == '1');
			}
			else if (kind == 'd' || kind == 'f') {                // Float or double
				curr.push(parseFloat(value));
			}
			else if (kind == 'i' || kind == 'u' || kind == 'e') { // Integer, unsigned or enum as int
				curr.push(parseInt(value));
			}
			else {                                                // Store anything else as a string
				curr.push(value);
			}
		}

		// Pop off all the arrays that have their values already
		while (m_count[m_count.length - 1] === 0) {
			m_stack.pop();
			m_count.pop();
			curr = m_stack[m_stack.length - 1];
		}
	});

	return root;
}

async function getPhotoMeta(mediaId) {
	// todo: build this url ourselves and find out what the parameters do
	const metaUrl = `https://www.google.com/maps/photometa/v1?authuser=0&hl=de&gl=at&pb=!1m4!1smaps_sv.tactile!11m2!2m1!1b1!2m2!1sde!2sat!3m5!1m2!1e10!2s${mediaId}!2m1!5s0x476d073f4daba163:0xfce7699ce9a8b1f4!4m61!1e1!1e2!1e3!1e4!1e5!1e6!1e8!1e12!1e17!2m1!1e1!4m1!1i48!5m1!1e1!5m1!1e2!6m1!1e1!6m1!1e2!9m36!1m3!1e2!2b1!3e2!1m3!1e2!2b0!3e3!1m3!1e3!2b1!3e2!1m3!1e3!2b0!3e3!1m3!1e8!2b0!3e3!1m3!1e1!2b0!3e3!1m3!1e4!2b0!3e3!1m3!1e10!2b1!3e2!1m3!1e10!2b0!3e3!11m2!3m1!4b1`;
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

	let smallMediaInfo;
	mediaInfoLoop: for (let obj of data) {
		for (let obj1 of obj) {
			if (!Array.isArray(obj1)) continue;
			if (!obj1.some(f => f.includes?.("googleusercontent.com"))) continue;
			smallMediaInfo = obj1;

			break mediaInfoLoop;
		}
	}

	if (!smallMediaInfo) {
		return { error: "fetch.fail" };
	}

	const mediaId = smallMediaInfo[0];
	const meta = await getPhotoMeta(mediaId);
	const actualMetadata = meta[1][0];

	const [, mediaType] = actualMetadata[2];
	console.log(mediaType);

	if (mediaType == MEDIA_TYPE_PHOTO) {
		const preview = actualMetadata[17][0][0];
		let finalUrl = preview.split("=")[0] + "=";
		finalUrl += "w0-"; // full resolution
		finalUrl += "ip";

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