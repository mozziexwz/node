import { test } from "node:test";
import assert from "node:assert/strict";
import { chromium, expect } from "@playwright/test";
import { build } from "esbuild";
import { fileURLToPath } from "node:url";

test("task details honor automatic-save policy and preserve existing manual copies", { timeout: 60000 }, async () => {
  const dir = fileURLToPath(new URL("../src/", import.meta.url));
  const app = await build({ entryPoints: [dir + "main.tsx"], bundle: true, write: false, outdir: "in-memory", format: "iife", platform: "browser" });
  const vault = await build({ stdin: { contents: "import {saveLocal,getLocal} from './vault'; window.auditSave=saveLocal;window.auditRead=getLocal;", resolveDir: dir }, bundle: true, write: false, format: "iife", platform: "browser" });
  const browser = await chromium.launch({ headless: true, ...(process.platform === "win32" ? { channel: "chrome" } : {}) });
  try {
    for (const [enabled, existing, count] of [[false,false,0],[false,true,1],[true,false,1]]) {
      const page = await browser.newPage();
      const errors = []; page.on("pageerror", e => errors.push(e.message));
      const base = "http://127.0.0.1:19983";
      const user = { id:"local-save-owner", role:"user", status:"active", email:"10000001@qq.com" };
      const task = { id:"task-save-policy", userId:user.id, kind:"deploy", state:"succeeded", configAvailable:true, createdAt:Date.now(), host:"8.8.8.8" };
      await page.route("**/*", route => {
        const url = new URL(route.request().url());
        if (url.origin !== base) return route.abort();
        if (url.pathname === "/") return route.fulfill({contentType:"text/html",body:'<div id="root"></div>'});
        const responses = { "/api/me":{user}, "/api/settings":{localSave:enabled,deploy:true,relay:true,dd:true}, "/api/tasks":{tasks:[task]}, ["/api/tasks/"+task.id+"/config"]:{synthetic:"no real credentials"} };
        return route.fulfill({json:responses[url.pathname] || {}});
      });
      await page.goto(base+"/#tasks");
      await page.addScriptTag({content:vault.outputFiles[0].text});
      if (existing) await page.evaluate(owner => window.auditSave(owner,"manual-existing",{keep:true},"manual.json"),user.id);
      await page.addScriptTag({content:app.outputFiles.find(f=>f.path.endsWith(".js")).text});
      await page.getByRole("button",{name:"详情",exact:true}).click();
      await expect(page.getByText(enabled ? /已存当前浏览器。配置已保存在/ : /仅本次可下载。配置已保存在/)).toBeVisible();
      const download = page.waitForEvent("download");
      await page.getByRole("button",{name:"下载配置",exact:true}).click();
      assert.ok((await download).suggestedFilename().endsWith(".json"));
      const stored = await page.evaluate(() => new Promise((resolve,reject) => {
        const open=indexedDB.open("msboost-production-vault-v1");open.onerror=()=>reject(open.error);
        open.onsuccess=()=>{const db=open.result;const request=db.transaction("files").objectStore("files").count();request.onsuccess=()=>{resolve(request.result);db.close();};request.onerror=()=>reject(request.error);};
      }));
      assert.equal(stored,count);
      if (existing) assert.deepEqual((await page.evaluate(owner=>window.auditRead(owner,"manual-existing"),user.id)).data,{keep:true});
      assert.deepEqual(errors,[]);
      await page.close();
    }
  } finally { await browser.close(); }
});
