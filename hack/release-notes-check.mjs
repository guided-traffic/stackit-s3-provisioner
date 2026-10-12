// Renders release notes with the semantic-release plugins installed in the
// current directory and the plugin configuration from .releaserc.json, without
// touching git or GitHub. It exists so that a pull request bumping the Node
// release tooling fails before the merge: the Semantic Release job runs only on
// main, after the merge, and an incompatible set of plugins there either fails
// every release or, worse, publishes notes without a single entry. Both have
// happened, so the check asserts on the rendered entries, not on exit status.
//
// Run it through `make release-notes-check`, which installs the dependencies.
import { createRequire } from "node:module";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { pathToFileURL } from "node:url";

const cwd = process.cwd();
const require = createRequire(join(cwd, "package.json"));
const load = async (name) => import(pathToFileURL(require.resolve(name)).href);

const releaserc = JSON.parse(readFileSync(join(cwd, ".releaserc.json"), "utf8"));
const pluginConfig = (name) => {
  const entry = releaserc.plugins.find((p) => (Array.isArray(p) ? p[0] : p) === name);
  if (!entry) throw new Error(`${name} is not configured in .releaserc.json`);
  return Array.isArray(entry) ? entry[1] ?? {} : {};
};

const silent = { log() {}, info() {}, warn() {}, error() {}, success() {} };
const commit = (hash, message) => ({
  hash: hash.repeat(40),
  commit: { long: hash.repeat(40), short: hash.repeat(7) },
  message,
  subject: message.split("\n")[0],
  body: "",
  committerDate: "2026-01-01T00:00:00Z",
});
const commits = [
  commit("a", "feat: add a feature"),
  commit("b", "fix(deps): fix a dependency"),
  commit("c", "chore: no release"),
];
const context = {
  cwd,
  env: {},
  logger: silent,
  commits,
  options: { repositoryUrl: "https://github.com/example/example.git" },
  lastRelease: { version: "1.0.0", gitTag: "v1.0.0", gitHead: "0".repeat(40) },
};

const { analyzeCommits } = await load("@semantic-release/commit-analyzer");
const type = await analyzeCommits(pluginConfig("@semantic-release/commit-analyzer"), context);
if (type !== "minor") throw new Error(`commit-analyzer returned ${type}, expected minor`);

const { generateNotes } = await load("@semantic-release/release-notes-generator");
const notes = await generateNotes(pluginConfig("@semantic-release/release-notes-generator"), {
  ...context,
  nextRelease: { type, version: "1.1.0", gitTag: "v1.1.0", gitHead: "a".repeat(40) },
});
for (const expected of ["add a feature", "fix a dependency"]) {
  if (!notes.includes(expected)) throw new Error(`release notes lack "${expected}":\n${notes}`);
}
console.log(`release type ${type}; release notes rendered:\n${notes}`);
