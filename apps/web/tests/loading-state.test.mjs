import { test } from 'node:test';
import assert from 'node:assert/strict';
import { chromium, expect } from '@playwright/test';
import { build } from 'esbuild';
import { fileURLToPath } from 'node:url';

test('settings cannot save false defaults while loading or after a failed refresh', {timeout:60000}, async () => {
  const compiled=await build({stdin:{contents:`import React from 'react';import {createRoot} from 'react-dom/client';import {Settings} from './admin';createRoot(document.getElementById('app')).render(<Settings onRefresh={()=>{}}/>);`,resolveDir:fileURLToPath(new URL('../src/',import.meta.url)),loader:'jsx'},bundle:true,write:false,format:'iife',platform:'browser'});
  const browser=await chromium.launch({headless:true,...(process.platform==='win32'?{channel:'chrome'}:{})});
  const page=await browser.newPage();const base='http://127.0.0.1:19988';let releaseRead,mode='pending',writes=[];
  const settings={deploy:true,relay:true,dd:true,paidCreate:true,planSale:true,cards:true,paywx:false,payali:false,monitor:true,localSave:true,publicArticles:true,attachments:true,maintenance:false,register:true,invite:false,registrationEmailVerificationRequired:true,freeToolsRequireVerifiedEmail:true,purchaseRequireVerifiedEmail:true};
  await page.route('**/*',async route=>{
    const url=new URL(route.request().url());if(url.origin!==base)return route.abort();
    if(url.pathname==='/')return route.fulfill({contentType:'text/html',body:'<div id="app"></div>'});
    if(url.pathname==='/api/admin/settings'){
      if(route.request().method()==='PUT'){writes.push(route.request().postDataJSON());mode='failed';return route.fulfill({json:{ok:true}});}
      if(mode==='pending')await new Promise(resolve=>releaseRead=resolve);
      if(mode!=='ready')return route.fulfill({status:503,json:{error:'设置读取失败，请重试'}});
      return route.fulfill({json:{settings}});
    }
    return route.fulfill({json:{}});
  });
  try{
    await page.goto(base);await page.addScriptTag({content:compiled.outputFiles[0].text});
    await expect.poll(()=>typeof releaseRead).toBe('function');
    await expect(page.getByRole('button',{name:'保存',exact:true})).toHaveCount(0);
    await expect(page.getByRole('checkbox')).toHaveCount(0);
    releaseRead();await expect(page.getByText('设置读取失败，请重试',{exact:true})).toBeVisible();
    await expect(page.getByRole('button',{name:'保存',exact:true})).toHaveCount(0);
    mode='ready';await page.getByRole('button',{name:'重新读取设置',exact:true}).click();
    await expect(page.getByRole('checkbox',{name:'部署 MSBOOST',exact:true})).toBeChecked();
    await page.getByRole('checkbox',{name:'维护模式',exact:true}).check();
    await page.getByRole('button',{name:'保存',exact:true}).click();
    await expect.poll(()=>writes.length).toBe(1);
    assert.equal(writes[0].maintenance,true);assert.equal(writes[0].deploy,true);assert.equal(writes[0].relay,true);assert.equal(writes[0].dd,true);assert.equal(writes[0].paywx,false);
    await expect(page.getByText('设置读取失败，请重试',{exact:true})).toBeVisible();
    await expect(page.getByRole('button',{name:'保存',exact:true})).toHaveCount(0);
    await expect(page.getByRole('checkbox')).toHaveCount(0);
    mode='ready';await page.getByRole('button',{name:'重新读取设置',exact:true}).click();
    await page.getByRole('button',{name:'注册与验证',exact:true}).click();
    await expect(page.getByRole('checkbox',{name:'注册时必须验证邮箱',exact:true})).toBeChecked();
    assert.equal(writes.length,1);
  }finally{releaseRead?.();await browser.close();}
});

test('pending and failed reads never claim disabled backups, missing nodes or empty tables', {timeout:60000}, async () => {
  const compiled=await build({stdin:{contents:`import React from 'react';import {createRoot} from 'react-dom/client';import {WholeSiteBackups} from './whole-site-backups';import {RouteBuilder} from './tunnels';import {ResourcePage,AdminRules} from './admin';import {Backups} from './backups';import {TasksPage} from './tools';createRoot(document.getElementById('app')).render(<><WholeSiteBackups/><RouteBuilder/><ResourcePage kind="executors"/><Backups/><TasksPage user={{id:'test',role:'admin'}} settings={{}}/><AdminRules/></>);`,resolveDir:fileURLToPath(new URL('../src/',import.meta.url)),loader:'jsx'},bundle:true,write:false,format:'iife',platform:'browser'});
  const browser=await chromium.launch({headless:true,...(process.platform==='win32'?{channel:'chrome'}:{})});
  const page=await browser.newPage();const pending=[];const errors=[];page.on('pageerror',e=>errors.push(e.message));
  const base='http://127.0.0.1:19986';let failed=true;
  const values={
    '/api/admin/disaster-backups':{schedule:{enabled:true,time:'12:01',zone:'Asia/Shanghai'},runs:[{updatedAt:Date.now(),localVerifiedAt:Date.now(),localOK:true,archive:'/root/test.tar.gz',size:1024,message:'备份完成'}]},
    '/api/admin/routes':{routes:[]},'/api/admin/relay-agents':{agents:[{id:'test-node',name:'Test node',address:'192.0.2.1',enabled:true}]},
    '/api/admin/executors':{executors:[{id:'test-executor',name:'Test executor',status:'active',online:true}]},
    '/api/admin/tasks':{tasks:[]},'/api/admin/user-rules':{rules:[]},
    '/api/admin/backups':{backups:[{id:'test-backup',size:1024,status:'verified',targets:{local:'verified'},createdAt:Date.now()}]},
  };
  await page.route('**/*',async route=>{
    const url=new URL(route.request().url());
    if(url.origin!==base)return route.abort();
    if(url.pathname==='/')return route.fulfill({contentType:'text/html',body:'<div id="app"></div>'});
    if(url.pathname in values){
      if(failed) {await new Promise(resolve=>pending.push(resolve));return route.fulfill({status:503,json:{error:'读取失败，请重试'}});}
      return route.fulfill({json:values[url.pathname]});
    }
    return route.fulfill({json:{}});
  });
  try {
    await page.goto(base);await page.addScriptTag({content:compiled.outputFiles[0].text});
    await expect(page.getByText('正在读取…',{exact:true})).toHaveCount(6);
    await expect(page.getByText('暂无记录',{exact:true})).toHaveCount(0);
    await expect(page.getByText(/每日计划：/)).toHaveCount(0);
    await expect(page.getByText(/请先在“节点”中新增/)).toHaveCount(0);
    await expect(page.getByRole('button',{name:'新增隧道'})).toBeDisabled();
    for(const resolve of pending)resolve();
    await expect(page.getByText('读取失败，请重试',{exact:true})).toHaveCount(6);
    await expect(page.getByText('正在读取…',{exact:true})).toHaveCount(0);
    await expect(page.getByText('暂无记录',{exact:true})).toHaveCount(0);
    await expect(page.getByText(/每日计划：/)).toHaveCount(0);
    failed=false;await page.getByRole('button',{name:'刷新状态'}).click();
    await expect(page.getByText(/每日计划：已启用/)).toBeVisible();
    await expect(page.getByText('/root/test.tar.gz（1.0 KiB）',{exact:true})).toBeVisible();
    await expect(page.getByText(/0.00 MB/)).toHaveCount(0);
    assert.deepEqual(errors,[]);
  } finally {for(const resolve of pending)resolve();await browser.close();}
});

test('backup schedule cannot erase remote targets while target reads are pending or failing', {timeout:60000}, async () => {
  const compiled=await build({stdin:{contents:`import React from 'react';import {createRoot} from 'react-dom/client';import {Backups} from './backups';createRoot(document.getElementById('app')).render(<Backups/>);`,resolveDir:fileURLToPath(new URL('../src/',import.meta.url)),loader:'jsx'},bundle:true,write:false,format:'iife',platform:'browser'});
  const browser=await chromium.launch({headless:true,...(process.platform==='win32'?{channel:'chrome'}:{})});
  const page=await browser.newPage();const base='http://127.0.0.1:19987';let releaseTargets,failed=true,saved=null;
  const plan={enabled:true,mode:'daily',time:'02:30',hours:24,weekday:0,timezone:'Asia/Shanghai',retentionDays:3,minCopies:2,targets:['remote-test']};
  await page.route('**/*',async route=>{
    const url=new URL(route.request().url());
    if(url.origin!==base)return route.abort();
    if(url.pathname==='/')return route.fulfill({contentType:'text/html',body:'<div id="app"></div>'});
    if(url.pathname==='/api/admin/backup-plan'){
      if(route.request().method()==='PUT')saved=route.request().postDataJSON();
      return route.fulfill({json:plan});
    }
    if(url.pathname==='/api/admin/backup-targets'){
      if(failed){await new Promise(resolve=>releaseTargets=resolve);return route.fulfill({status:503,json:{error:'目标读取失败，请重试'}});}
      return route.fulfill({json:{targets:[{id:'remote-test',name:'已选异地目标',enabled:true,testStatus:'passed'}]}});
    }
    return route.fulfill({json:{}});
  });
  try{
    await page.goto(base);await page.addScriptTag({content:compiled.outputFiles[0].text});
    await page.getByRole('button',{name:'业务备份计划',exact:true}).click();
    await expect(page.getByText('正在读取…',{exact:true})).toBeVisible();
    await expect(page.getByRole('button',{name:'保存',exact:true})).toHaveCount(0);
    releaseTargets();
    await expect(page.getByText('目标读取失败，请重试',{exact:true})).toBeVisible();
    await expect(page.getByRole('button',{name:'保存',exact:true})).toHaveCount(0);
    await page.getByRole('button',{name:'业务备份目标',exact:true}).click();
    await expect(page.getByText('暂无记录',{exact:true})).toHaveCount(0);
    await expect(page.getByRole('button',{name:'重新读取目标',exact:true})).toBeVisible();
    await page.getByRole('button',{name:'业务备份计划',exact:true}).click();
    failed=false;await page.getByRole('button',{name:'重新读取计划与目标',exact:true}).click();
    await expect(page.getByRole('checkbox',{name:'已选异地目标',exact:true})).toBeChecked();
    await page.getByRole('button',{name:'保存',exact:true}).click();
    await expect.poll(()=>saved?.targets).toEqual(['remote-test']);
    assert.equal(saved.enabled,true);
  }finally{releaseTargets?.();await browser.close();}
});
