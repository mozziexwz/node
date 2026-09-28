import {test} from 'node:test';
import assert from 'node:assert/strict';
import {chromium,expect} from '@playwright/test';
import {build} from 'esbuild';
import {fileURLToPath} from 'node:url';

test('whole-site history distinguishes schedule, local success, remote failure and opt-in alerts',async()=>{
 const compiled=await build({stdin:{contents:"import React from 'react';import {createRoot} from 'react-dom/client';import {WholeSiteBackups} from './whole-site-backups';createRoot(document.getElementById('app')).render(React.createElement(WholeSiteBackups));",resolveDir:fileURLToPath(new URL('../src/',import.meta.url)),loader:'jsx'},bundle:true,write:false,format:'iife',platform:'browser'});
 const browser=await chromium.launch({headless:true,...(process.platform==='win32'?{channel:'chrome'}:{})});
 const page=await browser.newPage();const writes=[];let enabled=false;const errors=[];page.on('pageerror',e=>errors.push(e.message));
 const stamp=Date.UTC(2026,8,28,1,2,3);
 await page.route('**/*',async route=>{
  const req=route.request(),url=new URL(req.url());
  if(url.origin!=='http://127.0.0.1:19918')return route.abort();
  if(url.pathname==='/')return route.fulfill({contentType:'text/html',body:'<div id="app"></div>'});
  if(url.pathname==='/api/admin/settings'){
   if(req.method()!=='GET'){writes.push(req.postDataJSON());enabled=req.postDataJSON().disasterBackupEmailEnabled;}
   return route.fulfill({json:{disasterBackupEmailEnabled:enabled}});
  }
  if(url.pathname==='/api/admin/disaster-backups')return route.fulfill({json:{schedule:{enabled:true,time:'12:01',zone:'Asia/Shanghai',nextRunAt:stamp+86400000},runs:[{id:'run',stage:'remote_failed',updatedAt:stamp,localVerifiedAt:stamp,localOK:true,remoteOK:false,remoteConfigured:true,message:'本地备份成功，异地上传失败',detail:'SSH 登录失败，请检查账号、密码',archive:'/root/msboost-backup/test.tar.gz',size:1024}]}});
  return route.fulfill({status:404,json:{error:'unexpected'}});
 });
 try{
  await page.goto('http://127.0.0.1:19918');await page.addScriptTag({content:compiled.outputFiles[0].text});
  await expect(page.getByText(/每日计划：已启用/)).toBeVisible();
  await expect(page.getByText(/最近异地校验成功：尚无成功记录/)).toBeVisible();
  await expect(page.getByText(/SSH 登录失败/)).toBeVisible();
  await expect(page.getByText('尚未成功',{exact:true})).toBeVisible();
  await page.getByLabel(/备份异常时发邮件/).check();await page.getByRole('button',{name:'保存提醒设置'}).click();
  await expect(page.getByLabel(/备份异常时发邮件/)).toBeChecked();
  assert.deepEqual(writes,[{disasterBackupEmailEnabled:true}]);assert.deepEqual(errors,[]);
 }finally{await browser.close();}
});
