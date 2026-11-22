// Runs after Redocly has built the page. It checks that the page needs nothing from another host, writes the Content-Security-Policy
// header of the page as an nginx snippet, and writes a gzip copy that nginx serves to clients that accept it.
// Usage: node finish-page.mjs <page>
import { createHash } from "node:crypto";
import { readFileSync, statSync, writeFileSync } from "node:fs";
import { dirname, join } from "node:path";
import { gzipSync, constants } from "node:zlib";

const page = process.argv[2];
if (!page) {
  console.error("usage: node finish-page.mjs <page>");
  process.exit(2);
}

const html = readFileSync(page, "utf8");
if (statSync(page).size < 100_000) {
  console.error(`${page} is too small for a built page, so the build did not finish`);
  process.exit(1);
}

// The page has to be one file. A script, style sheet or image from another host would break the policy below, so it fails the build.
// The text of the scripts and styles is removed first, because the Redoc code holds strings that look like tags.
const markup = html.replace(/(<(script|style)\b[^>]*>)[\s\S]*?<\/\2>/gi, "$1</$2>");
for (const tag of markup.match(/<(?:script|link|iframe|object|embed|img)\b[^>]*>/gi) ?? []) {
  if (/\b(?:src|href)\s*=\s*["']?(?:https?:)?\/\//i.test(tag)) {
    console.error(`The page loads something from another host, which the policy does not allow: ${tag.slice(0, 120)}`);
    process.exit(1);
  }
}

// Scripts are allowed by the hash of their text and nothing else. The page holds the Redoc code and the spec in one module script and a
// short script that sets the colour mode. Both change with the spec or the CLI version, so the hashes are made on every build.
const hashes = [...html.matchAll(/<script\b[^>]*>([\s\S]*?)<\/script>/gi)].map(
  (match) => `'sha256-${createHash("sha256").update(match[1], "utf8").digest("base64")}'`,
);
if (hashes.length === 0) {
  console.error("The page has no inline script, so the viewer would not start");
  process.exit(1);
}

// style-src has to allow inline styles. The page sets style attributes and styled-components adds rules at run time, and a hash covers
// neither. A style cannot run code, and the script rule above is what keeps injected script out.
// font-src allows data: because Redoc embeds two fonts in its CSS. img-src allows data: for the icons that are inline in the CSS.
// worker-src allows blob: for the search of the page. Everything else falls back to default-src 'none'.
const policy = [
  "default-src 'none'",
  `script-src ${hashes.join(" ")}`,
  "style-src 'unsafe-inline'",
  "img-src 'self' data:",
  "font-src data:",
  "worker-src blob:",
  "base-uri 'none'",
  "form-action 'none'",
  "frame-ancestors 'none'",
].join("; ");
const folder = dirname(page);
writeFileSync(join(folder, "csp.conf"), `add_header Content-Security-Policy "${policy}" always;\n`);

// Level 9 and a fixed header (Node writes no time stamp) give the same bytes for the same page, which the repeatable build check needs.
writeFileSync(`${page}.gz`, gzipSync(Buffer.from(html, "utf8"), { level: constants.Z_BEST_COMPRESSION }));
console.log(`${hashes.length} script hashes, policy of ${policy.length} bytes, page of ${html.length} bytes`);
