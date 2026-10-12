import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";
import test from "node:test";
import vm from "node:vm";

const source = await readFile(new URL("./background.js", import.meta.url), "utf8");
const runtimeStart = source.indexOf("chrome.runtime.onInstalled.addListener");
assert.notEqual(runtimeStart, -1);

function load() {
  const sent = [];
  const chrome = {
    debugger: {
      onDetach: { addListener() {} },
      async sendCommand(target, method, params) {
        sent.push({ method, url: params.url });
        return {};
      }
    },
    storage: { local: { async get() { return {}; }, async set() {} } }
  };
  const { BrowserBackend, checkNavigableUrl } = vm.runInNewContext(
    `${source.slice(0, runtimeStart)}\n({ BrowserBackend, checkNavigableUrl });`,
    { chrome, Date, URL, setTimeout, clearTimeout }
  );
  return { BrowserBackend, checkNavigableUrl, sent };
}

test("web pages and a plain about:blank may be opened", () => {
  const { checkNavigableUrl } = load();
  for (const url of ["https://example.test/a#b", "http://127.0.0.1:8080/", "about:blank", "about:blank?x"]) {
    checkNavigableUrl(url);
  }
});

test("URLs that crash Chrome under the debugger, and local files, are refused", () => {
  const { checkNavigableUrl } = load();
  for (const url of ["about:blank#probe", "about:blank#", "ABOUT:BLANK#x", "about:version", "chrome://settings", "file:///etc/passwd", "javascript:alert(1)", "example.test", undefined]) {
    assert.throws(() => checkNavigableUrl(url), /not allowed|invalid URL|requires a url/, String(url));
  }
});

test("executeCdp refuses the navigation before it reaches the debugger", async () => {
  const { BrowserBackend, sent } = load();
  const backend = new BrowserBackend();
  await backend.store.ready;
  backend.requireSession = async () => ({});
  await assert.rejects(
    backend.executeCdp({ target: {}, method: "Page.navigate", commandParams: { url: "about:blank#probe" } }),
    /about: URLs is not allowed/
  );
  await assert.rejects(
    backend.executeCdp({ target: {}, method: "Target.createTarget", commandParams: { url: "chrome://version" } }),
    /chrome: URLs is not allowed/
  );
  await backend.executeCdp({ target: {}, method: "Page.navigate", commandParams: { url: "https://example.test/" } });
  assert.deepEqual(sent.map((c) => c.url), ["https://example.test/"]);
});
