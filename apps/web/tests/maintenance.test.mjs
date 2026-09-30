import {test} from 'node:test';
import assert from 'node:assert/strict';
import {chromium, expect} from '@playwright/test';
import {build} from 'esbuild';
import {fileURLToPath} from 'node:url';

test('maintenance UI explains draining, scoped cleanup and verified backup targets', {timeout:60000}, async () => {
  const source=fileURLToPath(new URL('../src/main.tsx', import.meta.url));
  const bundle=await build({entryPoints:[source],bundle:true,write:false,outdir:'in-memory',format:'iife',platform:'browser'});
  const browser=await chromium.launch({headless:true,...(process.platform==='win32'?{channel:'chrome'}:{})});
  const page=await browser.newPage();
  const errors=[];page.on('pageerror',error=>errors.push(error.message));
  let testStatus='', attempts=0;
  const base='http://127.0.0.1:19984';
  try {
    await page.route('**/*',route=>{
      const url=new URL(route.request().url());
      if(url.origin!==base)return route.abort();
      if(url.pathname==='/')return route.fulfill({contentType:'text/html',body:'<div id="root"></div>'});
      if(url.pathname==='/api/admin/backup-targets/fixture-target/test') {
        attempts++;testStatus=attempts===1?'failed':'passed';
        return route.fulfill(attempts===1?{status:502,json:{error:'测试失败：请核对 SSH 密码或私钥'}}:{json:{ok:true,message:'SSH、写入和校验回读通过，测试文件已清理'}});
      }
      const responses={
        '/api/me':{user:{id:'maintenance-admin',role:'admin',status:'active',email:'10000001@qq.com'}},
        '/api/settings':{},
        '/api/admin/executors':{executors:[{id:'fixture-executor',name:'验收执行机',status:'draining',online:true,version:'v2.2.2',lastSeenAt:Date.now()}]},
        '/api/admin/backups':{backups:[]},
        '/api/admin/backup-plan':{},
        '/api/admin/backup-capacity':{status:'normal',estimatedPackedBytes:1048576,maxPackedBytes:104857600,remainingContentBytes:82837504},
        '/api/admin/backup-targets':{targets:[{id:'fixture-target',name:'验收备份',host:'192.0.2.1',path:'/root/test-backup',enabled:true,testStatus,testedAt:testStatus?Date.now():0}]}
      };
      return route.fulfill({json:responses[url.pathname]||{}});
    });
    await page.goto(base+'/#executors');
    await page.addScriptTag({content:bundle.outputFiles.find(file=>file.path.endsWith('.js')).text});
    await expect(page.getByText('等待现有任务完成（不接新任务）',{exact:true})).toBeVisible();
    await expect(page.getByText('v2.2.2',{exact:true})).toBeVisible();
    await page.getByRole('button',{name:'本机清理说明',exact:true}).click();
    const modal=page.locator('dialog');
    await expect(modal.getByText('卸载本机执行机',{exact:true})).toBeVisible();
    await expect(modal.locator('pre').first()).toContainText('--capability executor --check');
    await expect(modal.locator('pre').last()).toHaveText('bash /tmp/msboost-cleanup-agent.sh --capability executor --cleanup');
    await expect(modal).toContainText('保留私有备份、共享程序及同机另一个角色');
    await modal.getByRole('button',{name:'关闭窗口'}).click();
    await page.getByRole('button',{name:'备份与恢复',exact:true}).click();
    await expect(page.getByText(/可恢复备份预计/)).toBeVisible();
    await page.getByRole('button',{name:'业务备份目标',exact:true}).click();
    await expect(page.getByText('尚未测试',{exact:true})).toBeVisible();
    await page.getByRole('button',{name:'测试连接及回读'}).click();
    await expect(page.getByText('最近测试失败',{exact:true})).toBeVisible();
    await expect(page.getByText('测试失败：请核对 SSH 密码或私钥',{exact:true})).toBeVisible();
    await page.getByRole('button',{name:'测试连接及回读'}).click();
    await expect(page.getByText('最近测试通过',{exact:true})).toBeVisible();
    assert.equal(attempts,2);assert.deepEqual(errors,[]);
  } finally {await browser.close();}
});
