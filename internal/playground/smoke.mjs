// Loads the built playground the way the page does and calls its exports.
//
// Compiling for js/wasm proves only that the Go builds. This proves the bridge:
// that the module installs its functions, that a translation comes back with the
// shape app.js renders, and that a bad query comes back as a value rather than
// taking the runtime down with it.
//
// Usage: node internal/playground/smoke.mjs web

import fs from "node:fs";
import path from "node:path";
import { pathToFileURL } from "node:url";

const webDir = process.argv[2] || "web";

function fail(message) {
  console.error("smoke: " + message);
  process.exit(1);
}

for (const name of ["wasm_exec.js", "polyql.wasm"]) {
  if (!fs.existsSync(path.join(webDir, name))) {
    fail(`${path.join(webDir, name)} is missing — run \`make playground\` first`);
  }
}

// wasm_exec.js is a script, not a module: importing it by file URL runs it and
// leaves Go on the global object, which is what the page relies on too.
await import(pathToFileURL(path.resolve(webDir, "wasm_exec.js")).href);
if (typeof globalThis.Go !== "function") {
  fail("wasm_exec.js did not define Go");
}

let ready = false;
globalThis.polyqlReady = () => { ready = true; };

const go = new globalThis.Go();
const { instance } = await WebAssembly.instantiate(
  fs.readFileSync(path.resolve(webDir, "polyql.wasm")),
  go.importObject,
);
go.run(instance);

// go.run returns once the program blocks, but the exports are installed by then.
if (!ready) {
  fail("polyqlReady was never called" +
    (globalThis.polyqlLoadError ? " — " + globalThis.polyqlLoadError : ""));
}

const languages = globalThis.polyqlLanguages();
for (const expected of ["promql", "logql", "traceql"]) {
  if (!languages.includes(expected)) {
    fail(`the module does not offer ${expected} (got: ${languages.join(", ")})`);
  }
}
console.log("languages: " + languages.join(", "));

// A translation the target can express exactly, and one it cannot: the second is
// not an error, and the report is where it has to show up.
const cases = [
  { from: "promql", to: "logql", query: 'rate(http_requests_total[5m])', expectOK: true },
  { from: "traceql", to: "logql", query: '{resource.service.name = "web" && duration > 100ms}', expectOK: true },
  { from: "promql", to: "logql", query: 'histogram_quantile(0.99, x)', expectOK: true, expectUnsupported: true },
  { from: "promql", to: "logql", query: 'this is not a query(', expectOK: false },
];

for (const c of cases) {
  const result = globalThis.polyqlTranslate(c.from, c.to, c.query);
  if (result.ok !== c.expectOK) {
    fail(`${c.from}→${c.to} ${JSON.stringify(c.query)}: expected ok=${c.expectOK}, got ${JSON.stringify(result)}`);
  }
  if (!result.ok) {
    console.log(`${c.from}→${c.to}: parse error reported as a value: ${result.error}`);
    continue;
  }
  if (typeof result.report?.score !== "number") {
    fail(`${c.from}→${c.to}: result carries no fidelity score`);
  }
  if (c.expectUnsupported && result.report.unsupported === 0) {
    fail(`${c.from}→${c.to} ${JSON.stringify(c.query)}: expected an unsupported construct, report said none`);
  }
  console.log(`${c.from}→${c.to}: ${result.output || "(nothing emitted)"}  [score ${result.report.score.toFixed(2)}]`);
}

// Calling it wrong must return a value too: a panic across the js boundary takes
// the exported functions with it, and the page would go silent rather than show
// an error.
const arity = globalThis.polyqlTranslate("promql");
if (arity.ok !== false || !arity.error) {
  fail("a call with too few arguments did not come back as an error value");
}

console.log("smoke: ok");
process.exit(0);
