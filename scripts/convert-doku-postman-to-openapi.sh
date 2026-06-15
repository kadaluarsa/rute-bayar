#!/usr/bin/env bash
set -euo pipefail

OUTPUT_FILE="${1:-docs/apis/doku-openapi-from-postman.json}"
shift || true

if [ "$#" -gt 0 ]; then
  SOURCE_FILES=("$@")
elif [ -n "${DOKU_POSTMAN_COLLECTION_URL-}" ]; then
  SOURCE_FILES=("$DOKU_POSTMAN_COLLECTION_URL")
else
  echo "usage: convert-doku-postman-to-openapi.sh <output> <postman.json>" >&2
  echo "example: ./scripts/convert-doku-postman-to-openapi.sh docs/apis/doku-openapi-from-postman.json /path/doku-postman-collection.json" >&2
  echo "or set DOKU_POSTMAN_COLLECTION_URL env and run: ./scripts/convert-doku-postman-to-openapi.sh" >&2
  exit 1
fi

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT_PATH="$OUTPUT_FILE"
if [[ "$OUT_PATH" != /* ]]; then
  OUT_PATH="$ROOT_DIR/$OUT_PATH"
fi
mkdir -p "$(dirname "$OUT_PATH")"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

NODE_SCRIPT="$TMP_DIR/convert.js"
cat > "$NODE_SCRIPT" <<'NODE'
const fs = require('fs');

function isLikelyIdSegment(value) {
	if (!value) {
		return false;
	}

	if (/^[0-9a-f]{12,}$/i.test(value)) {
		return true;
	}
	if (/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(value)) {
		return true;
	}
	if (/^[a-z0-9_]+[_-][0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i.test(value)) {
		return true;
	}
	if (/^order-id-\d+$/i.test(value)) {
		return true;
	}
	if (/^[a-z0-9_]+_[0-9a-f]{12,}(?:-[0-9a-f]{4})*$/i.test(value)) {
		return true;
	}
	return false;
}

function isLikelyTemplateValue(value) {
	if (!value) {
		return false;
	}

	if (/^(INV|SAC|ORD|ORDER|REQUEST|REFERENCE)[-_][0-9a-z-]+$/i.test(value)) {
		return true;
	}
	if (/^[A-Z]{2,}[A-Z0-9_-]*-[0-9a-f]{8,}$/i.test(value)) {
		return true;
	}
	if (/^[A-Z]{2,}-\d{3,}$/i.test(value)) {
		return true;
	}
	if (/^[A-Z0-9_]+-[0-9a-f]{12,}$/i.test(value)) {
		return true;
	}
	return false;
}

function toPath(rawUrl) {
	if (typeof rawUrl !== 'string' || rawUrl.trim() === '') {
		return '';
	}

	let pathValue = rawUrl
		.replace(/^[a-zA-Z]+\s+/, '')
		.replace(/^\{\{[^}]+\}\}\//, '/')
		.replace(/\{\{[^}]+\}\}/g, '');
	pathValue = pathValue.replace(/^https?:\/\/[^/]+/i, '');
	pathValue = pathValue.split('?')[0];

	if (pathValue.includes('your endpoint here') || pathValue.includes('your endpoint')) {
		return '';
	}
	if (/\s/.test(pathValue)) {
		return '';
	}

	let segments = pathValue
		.split('/')
		.map((segment) => {
			const trimmed = segment.trim();
			if (!trimmed) {
				return '';
			}

			if (trimmed.startsWith(':')) {
				return `{${trimmed.slice(1)}}`;
			}

			if (/^\{[^}]+\}$/.test(trimmed)) {
				return trimmed;
			}

		const templateMatch = trimmed.match(/^([^=]+)=.*$/);
			if (templateMatch) {
				const key = templateMatch[1].trim();
				if (key) {
					return `${key}={${key.replace(/[^a-zA-Z0-9_]+/g, '_')}}`;
				}
			}

			if (/\.(js|css|html|png|jpg|jpeg|gif|svg|map)$/i.test(trimmed)) {
				return '';
			}

			if (isLikelyIdSegment(trimmed) || isLikelyTemplateValue(trimmed) || /^\d+$/.test(trimmed)) {
				return '{id}';
			}

			return trimmed;
		});

	segments = segments.filter((segment) => segment.length > 0);
	if (segments.length === 0) {
		return '/';
	}

	return `/${segments.join('/')}`;
}

function collectItems(root) {
	const output = [];
	const walk = (nodes) => {
		if (!Array.isArray(nodes)) {
			return;
		}

		for (const node of nodes) {
			if (!node || typeof node !== 'object') {
				continue;
			}

			if (Array.isArray(node.item)) {
				walk(node.item);
				continue;
			}

			const request = node.request;
			if (!request || !request.url) {
				continue;
			}
			const method = String(request.method || 'GET').toUpperCase();
			const rawUrl =
				typeof request.url === 'string'
					? request.url
					: (typeof request.url.raw === 'string' ? request.url.raw : '');
			if (!rawUrl) {
				continue;
			}

			const path = toPath(rawUrl);
			if (!path) {
				continue;
			}

			output.push({
				method,
				path,
				name: node.name || `${method} ${path}`,
				key: `${method.toLowerCase()} ${path}`,
			});
		}
	};

	walk(root.item);
	return output;
}

function extractFromSource(source) {
	const rawData = fs.readFileSync(source, 'utf8');
	const collection = JSON.parse(rawData);
	return collectItems(collection);
}

const outputPath = process.argv[2];
const sources = process.argv.slice(3);
const items = [];

if (!outputPath) {
	throw new Error('output path is required');
}

if (sources.length === 0) {
	throw new Error('at least one postman source file is required');
}

for (const sourcePath of sources) {
	for (const item of extractFromSource(sourcePath)) {
		items.push(item);
	}
}

items.sort((a, b) => {
	const pathCmp = a.path.localeCompare(b.path);
	if (pathCmp !== 0) {
		return pathCmp;
	}
	return a.method.localeCompare(b.method);
});

const unique = new Map();
for (const item of items) {
	if (!unique.has(item.key)) {
		unique.set(item.key, item);
	}
}

const paths = {};
for (const item of unique.values()) {
	if (!paths[item.path]) {
		paths[item.path] = {};
	}

	const summaryBase = item.name.replace(/\s+/g, ' ').trim();
	const pathTag = item.path.replace(/^\//, '').replace(/\//g, '_') || 'root';
	const operationId = `${item.method.toLowerCase()}_${pathTag}_${summaryBase.toLowerCase().replace(/[^a-z0-9]+/g, '_')}`.slice(0, 96);

	paths[item.path][item.method.toLowerCase()] = {
		operationId,
		summary: item.name,
		responses: {
			'200': { description: 'Success' },
			default: { description: 'Error' },
		},
	};
}

const spec = {
	openapi: '3.0.3',
	info: {
		title: 'DOKU API (from Postman collection)',
		version: '2026-06-14',
		description: 'Converted from DOKU Postman collection. Validate and enrich before production use.',
	},
	servers: [
		{ url: 'https://api-sandbox.doku.com', description: 'DOKU Sandbox' },
		{ url: 'https://api.doku.com', description: 'DOKU Production' },
	],
	paths,
};

fs.writeFileSync(outputPath, JSON.stringify(spec, null, 2) + '\n');
NODE

if [ -n "${DOKU_POSTMAN_COLLECTION_URL-}" ] && [ ${#SOURCE_FILES[@]} -eq 1 ] && [ "${SOURCE_FILES[0]}" = "$DOKU_POSTMAN_COLLECTION_URL" ]; then
	curl -fsSL "$DOKU_POSTMAN_COLLECTION_URL" -o "$TMP_DIR/postman.json"
	node "$NODE_SCRIPT" "$OUT_PATH" "$TMP_DIR/postman.json"
else
	node "$NODE_SCRIPT" "$OUT_PATH" "${SOURCE_FILES[@]}"
fi
printf 'wrote %s\n' "$OUT_PATH"
