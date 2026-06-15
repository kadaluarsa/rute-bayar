#!/usr/bin/env bash
set -euo pipefail

SPEC_FILE="${1:-docs/apis/doku-openapi-from-postman.json}"
OUT_FILE="${2:-internal/cli/doku_openapi_aliases_generated.go}"
VAR_NAME="${3:-generatedDokuAPIOperationAliases}"

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ "$SPEC_FILE" = /* ]]; then
	SPEC_PATH="$SPEC_FILE"
else
	SPEC_PATH="$ROOT_DIR/$SPEC_FILE"
fi

if [[ "$OUT_FILE" = /* ]]; then
	OUT_PATH="$OUT_FILE"
else
	OUT_PATH="$ROOT_DIR/$OUT_FILE"
fi

mkdir -p "$(dirname "$OUT_PATH")"

node - "$SPEC_PATH" "$OUT_PATH" "$VAR_NAME" <<'NODE'
const fs = require('fs');

const specPath = process.argv[2];
const outPath = process.argv[3];
const varName = process.argv[4] || 'generatedOpenAPIOperationAliases';
if (!specPath || !outPath) {
	throw new Error('usage: generate-doku-openapi-aliases.sh <spec-path> <out-go-file> [var-name]');
}

const rawSpec = fs.readFileSync(specPath, 'utf8');
const spec = JSON.parse(rawSpec);
const HTTP_METHODS = new Set(['get', 'post', 'put', 'patch', 'delete', 'head', 'options', 'connect', 'trace']);

function sanitizeAlias(value) {
	return value
		.toLowerCase()
		.replace(/[^a-z0-9]+/g, '-')
		.replace(/-+/g, '-')
		.replace(/^-|-$/g, '');
}

function withoutVersions(segments) {
	return segments.filter((segment) => {
		if (!segment) {
			return false;
		}
		const isPlaceholder = segment.startsWith('{') && segment.endsWith('}');
		if (isPlaceholder) {
			return false;
		}
		if (/^v\d+$/i.test(segment)) {
			return false;
		}
		if (/^v\d+(?:\.\d+)+$/i.test(segment)) {
			return false;
		}
		if (/^\d+$/.test(segment)) {
			return false;
		}
		return true;
	});
}

function maybeSingularize(value) {
	if (!value || value.length <= 3 || !value.endsWith('s')) {
		return '';
	}
	if (value.endsWith('ss') || value.endsWith('us') || value === 'status') {
		return '';
	}
	return value.slice(0, -1);
}

function isSnapRelatedPath(segments) {
	return segments.some((segment) => String(segment || '').toLowerCase().includes('snap'));
}

function buildAliases(path, method) {
	const normalized = String(path || '').split('?')[0].trim();
	if (!normalized) {
		return [];
	}

	const methodLower = String(method || '').toLowerCase();
	const segments = normalized
		.split('/')
		.map((segment) => segment.trim())
		.filter((segment) => segment.length > 0);

	const aliases = new Map();
	const addAlias = (alias, isExact) => {
		if (!alias) {
			return;
		}

		const existing = aliases.get(alias);
		if (!existing || (!existing.isExact && isExact)) {
			aliases.set(alias, { isExact });
		}
	};
	const allVariants = [
		{ segments, isExact: true },
		{ segments: withoutVersions(segments), isExact: false },
	];

	for (const variant of allVariants) {
		const variantSegments = variant.segments;
		if (variantSegments.length === 0) {
			continue;
		}

		const variantIsExact = variant.isExact;
		const compact = [...new Set(variantSegments)];
		if (compact.length === 0) {
			continue;
		}

		const baseAlias = sanitizeAlias(compact.join('-'));
		if (baseAlias) {
			addAlias(baseAlias, variantIsExact);
			const baseSingular = maybeSingularize(baseAlias);
			if (baseSingular) {
				addAlias(baseSingular, false);
			}
		}

		const lastAlias = sanitizeAlias(compact[compact.length - 1]);
		if (lastAlias) {
			addAlias(lastAlias, variantIsExact);
			const singularLast = maybeSingularize(lastAlias);
			if (singularLast) {
				addAlias(singularLast, false);
			}

			if (methodLower) {
				addAlias(`${methodLower}-${lastAlias}`, variantIsExact);
				if (singularLast) {
					addAlias(`${methodLower}-${singularLast}`, false);
				}
			}
		}
	}

	if (methodLower) {
		for (const [alias, { isExact }] of Array.from(aliases)) {
			if (alias.startsWith(`${methodLower}-`)) {
				continue;
			}
			addAlias(`${methodLower}-${alias}`, isExact);
		}
	}

	return [...aliases.entries()].map(([alias, meta]) => ({ alias, isExact: meta.isExact }));
}

const aliasTargets = new Map();
const generated = new Map();
for (const [rawPath, methods] of Object.entries(spec.paths || {})) {
	if (typeof methods !== 'object' || methods === null) {
		continue;
	}

	for (const [rawMethod, operation] of Object.entries(methods)) {
		if (!HTTP_METHODS.has(rawMethod)) {
			continue;
		}
		if (typeof operation !== 'object' || operation === null) {
			continue;
		}

		if (!rawPath || rawPath === '/') {
			continue;
		}
		const normalizedPathSegments = String(rawPath)
			.split('/')
			.filter((segment) => segment.length > 0);
		// Skip non-nested paths (single-segment routes) while allowing
		// nested provider endpoints such as /{id}/v1/accounts.
		if (normalizedPathSegments.length <= 1) {
			continue;
		}

		if (isSnapRelatedPath(normalizedPathSegments)) {
			continue;
		}

		const method = rawMethod.toUpperCase();
		if (!method) {
			continue;
		}

		const methodPathKey = `${method} ${rawPath}`;
		for (const { alias, isExact } of buildAliases(rawPath, method)) {
			const existing = aliasTargets.get(alias);
			if (!existing) {
				aliasTargets.set(alias, [{ methodPath: methodPathKey, isExact }]);
				continue;
			}

			existing.push({ methodPath: methodPathKey, isExact });
		}
	}
}

for (const [alias, entries] of aliasTargets.entries()) {
	if (entries.length === 1) {
		const methodPath = entries[0].methodPath;
		const separatorIndex = methodPath.indexOf(' ');
		const method = methodPath.slice(0, separatorIndex);
		const path = methodPath.slice(separatorIndex + 1);
		generated.set(alias, { method, path });
		continue;
	}

	const exactEntries = entries.filter((entry) => entry.isExact);
	if (exactEntries.length !== 1) {
		continue;
	}

	const methodPath = exactEntries[0].methodPath;
	const separatorIndex = methodPath.indexOf(' ');
	const method = methodPath.slice(0, separatorIndex);
	const path = methodPath.slice(separatorIndex + 1);
	generated.set(alias, { method, path });
}

function formatGoString(value) {
	return JSON.stringify(value);
}

const entries = [...generated.entries()].sort(([a], [b]) => a.localeCompare(b));
let output = '// Code generated by scripts/generate-doku-openapi-aliases.sh. Do NOT EDIT.\n';
output += '\n';
output += 'package cli\n\n';
output += `var ${varName} = map[string]pathTemplateOperation{\n`;

for (const [alias, value] of entries) {
	output += `\t${formatGoString(alias)}: {method: ${formatGoString(value.method)}, path: ${formatGoString(value.path)},},\n`;
}

output += '}\n';
fs.writeFileSync(outPath, output);
console.log(`wrote ${outPath}`);
NODE

if command -v gofmt >/dev/null 2>&1; then
	gofmt -w "$OUT_PATH"
fi
