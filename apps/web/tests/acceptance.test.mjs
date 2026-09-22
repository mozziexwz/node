import { test } from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import { chromium, expect } from '@playwright/test';

test('landing title: two intact lines, orange second line, responsive layout', {timeout: 30000}, async () => {
  const base = process.env.MSBOOST_TEST_URL || 'http://127.0.0.1:8080';
  assert.match(base, /^http:\/\/(127\.0\.0\.1|localhost):\d+$/);
  const browser = await chromium.launch({headless:true, ...(process.platform === 'win32' ? {channel:'chrome'} : {})});
  const page = await browser.newPage({baseURL:base});
  const errors=[];
  page.on('pageerror',error=>errors.push(error.message));
  // Public landing fixture only: no login, credentials, account mutation or
  // external API calls are needed to verify the complete page composition.
  await page.route('**/api/**', route=>{
    const path=new URL(route.request().url()).pathname;
    return path==='/api/me'
      ? route.fulfill({status:401,json:{error:'not signed in'}})
      : route.fulfill({json:path==='/api/health'?{status:'ok'}:{}});
  });
  try {
    await page.goto(base);
    const title=page.getByRole('heading',{level:1});
    await expect(title.locator(':scope > span')).toHaveText(['一键部署你的','独立IP游戏节点']);
    await page.evaluate(()=>document.fonts.ready);
    const maples=page.locator('.brand > svg, .hero-steps .active > svg');
    await expect(maples).toHaveCount(2);
    for(const maple of await maples.all()) {
      const geometry=await maple.evaluate(svg=>{
        const box=svg.getBBox();
        return {x:box.x,y:box.y,right:box.x+box.width,bottom:box.y+box.height,
          strokes:[...svg.querySelectorAll('path')].map(path=>getComputedStyle(path).stroke)};
      });
      assert.deepEqual(geometry.strokes,['rgb(0, 0, 0)','rgb(0, 0, 0)'],'black leaf and semicircle on both white and orange backgrounds');
      assert.ok(geometry.x>=2 && geometry.y>=2 && geometry.right<=62 && geometry.bottom<=62,'maple fits inside the viewBox with stroke padding');
    }
    await page.locator('.brand').first().screenshot({path:'../../.runtime/supplement23-brand.png'});
    await page.locator('.hero-steps .active').screenshot({path:'../../.runtime/supplement23-logo-step.png'});
    for(const width of [1440,1050,900,784,761,760,375,320]) {
      await page.setViewportSize({width,height:900});
      const layout=await title.evaluate(element=>{
        const box=element.getBoundingClientRect();
        const lines=[...element.children].map(line=>{
          const range=document.createRange();range.selectNodeContents(line);
          const rects=[...range.getClientRects()];
          const rect=rects[0];
          return {text:line.textContent,count:rects.length,left:rect.left,right:rect.right,top:rect.top,bottom:rect.bottom,color:getComputedStyle(line).color};
        });
        const hero=element.closest('.hero');
        return {lines,left:box.left,right:box.right,height:box.height,lineHeight:parseFloat(getComputedStyle(element).lineHeight),columns:getComputedStyle(hero).gridTemplateColumns.split(' ').length,overflow:document.documentElement.scrollWidth>innerWidth};
      });
      assert.equal(layout.lines.length,2,`two title lines at ${width}px`);
      for(const line of layout.lines) {
        assert.equal(line.count,1,`no third line at ${width}px: ${line.text}`);
        assert.ok(line.left>=layout.left-1 && line.right<=layout.right+1,`title remains inside its column at ${width}px`);
      }
      assert.ok(Math.abs(layout.lines[1].top-layout.lines[0].top-layout.lineHeight)<1,`explicit separate line baselines at ${width}px`);
      assert.ok(Math.abs(layout.height-2*layout.lineHeight)<1,`exactly two line boxes at ${width}px`);
      assert.equal(layout.lines[0].color,'rgb(38, 38, 38)','first line retains approved ink');
      assert.equal(layout.lines[1].color,'rgb(231, 101, 39)','entire second line uses approved orange');
      assert.equal(layout.columns,width>760?2:1,`preserve responsive page columns at ${width}px`);
      assert.equal(layout.overflow,false,`complete page has no horizontal overflow at ${width}px`);
      await expect(page.locator('.auth-card')).toBeVisible();
      await expect(page.locator('.public-tools')).toBeVisible();
      await expect(page.locator('.public-footer')).toBeVisible();
      if([1440,784,375].includes(width)) await page.screenshot({path:`../../.runtime/hero-title-${width}.png`,fullPage:true});
    }
    assert.deepEqual(errors,[]);
  } finally {await browser.close();}
});

// Requires scripts/dev-server.mjs. All accounts/data below belong to the
// isolated local acceptance server, never to a production deployment.
test('real server: browser permissions, admin pages, content and account flows', {timeout: 120000}, async () => {
  const base = process.env.MSBOOST_TEST_URL || 'http://127.0.0.1:8080';
  assert.match(base, /^http:\/\/(127\.0\.0\.1|localhost):\d+$/);
  const admin = JSON.parse(fs.readFileSync(process.env.MSBOOST_TEST_CREDENTIALS || '../../.runtime/acceptance-credentials.json', 'utf8'));
  const browser = await chromium.launch({headless:true, ...(process.platform === 'win32' ? {channel:'chrome'} : {})});
  const ctx = await browser.newContext({baseURL:base, viewport:{width:1440,height:1000}});
  const page = await ctx.newPage(), errors=[];
  page.on('pageerror', e=>errors.push(e.message));
  async function request(path, data, method='POST', context=ctx) {
    const me = await context.request.get('/api/me');
    const token = me.ok() ? (await me.json()).csrfToken : '';
    return context.request.fetch(path,{method,data,headers:{'Origin':base,'X-CSRF-Token':token}});
  }
  try {
    await page.goto(base);
    await expect(page.getByRole('heading',{level:1})).toContainText('一键部署你的');
    await expect(page.getByLabel('QQ 邮箱')).toHaveValue('');
    assert.equal(await page.getByRole('checkbox').isChecked(),false);
    await page.goto(base+'/#home');
    await page.getByLabel('QQ 邮箱').fill(admin.email);
    await page.getByLabel('密码',{exact:true}).fill(admin.password);
    await page.getByRole('checkbox').check();
    await page.locator('form').getByRole('button',{name:'登录',exact:true}).click();
    await expect(page.locator('.sidebar')).toBeVisible();
    await expect(page).toHaveURL(/#tutorials$/);
    await expect(page.locator('.side-nav').getByRole('heading',{name:'免费部署工具',exact:true})).toBeVisible();
    await expect(page.locator('.side-nav').getByRole('heading',{name:'捐赠权益管理',exact:true})).toBeVisible();
    assert.ok((await page.locator('.nav-group').allTextContents()).findIndex(t=>t.includes('免费部署工具'))<(await page.locator('.nav-group').allTextContents()).findIndex(t=>t.includes('捐赠权益管理')));
    const me=await (await ctx.request.get('/api/me')).json();
    assert.equal(me.user.role,'admin');assert.ok(!me.user.passwordHash);
    assert.equal((await ctx.request.post('/api/admin/plans',{data:{}})).status(),403,'CSRF rejects missing token');
    for (const label of ['部署 MSBOOST','配置中转服务器','DD 系统','节点','隧道']) await expect(page.locator('.sidebar').getByRole('button',{name:label,exact:true})).toBeVisible();
    for (const hash of ['home','users','tasks','executors','agents','routes','rules','plans','cards','invitations','orders','payments','settings','backups','tickets','account']) {
      await page.goto(base+'/#'+hash);
      await expect(page.locator('.sidebar')).toBeVisible();
      await expect(page.locator('main')).toBeVisible();
      await expect(page.locator('main')).not.toContainText('正在初始化');
      assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false,'desktop overflow '+hash);
    }
    await page.goto(base+'/#backups');
    await page.getByRole('button',{name:'恢复备份',exact:true}).click();
    await expect(page.getByRole('heading',{name:'安全恢复（网页）',exact:true})).toBeVisible();
    await expect(page.getByRole('heading',{name:'完整灾难恢复（离线）',exact:true})).toBeVisible();
    await expect(page.getByRole('button',{name:'确认安全恢复',exact:true})).toHaveCount(0);
    await page.screenshot({path:'../../.runtime/remediation-backups.png',fullPage:true});
    await page.goto(base+'/#users');
    for (const heading of ['邮箱验证','枫叶','剩余天数','总流量（GB）','已用流量（GB）','规则速率（Mbps）']) await expect(page.getByRole('columnheader',{name:heading})).toBeVisible();
    await page.getByRole('button',{name:'添加用户',exact:true}).click();
    await expect(page.getByRole('dialog')).toBeVisible();
    const dialogBox=await page.getByRole('dialog').boundingBox();
    assert.ok(dialogBox.x>0 && dialogBox.y>=0 && dialogBox.y+dialogBox.height<=1000,'dialog remains within viewport');
    await expect(page.getByLabel('权益剩余天数')).toBeVisible();
    await page.getByRole('button',{name:'关闭窗口'}).click();
    await page.goto(base+'/#tutorials');
    await page.getByRole('button',{name:'新建文章',exact:true}).click();
    await page.getByRole('button',{name:'全屏编辑',exact:true}).click();
    const editor=page.getByRole('region',{name:'Markdown 编辑器'});
    await expect(editor.getByRole('button',{name:'保存',exact:true})).toBeVisible();
    await expect(editor.locator('.markdown-toolbar')).toBeVisible();
    const expandedBox=await editor.boundingBox();
    assert.ok(expandedBox.width>=1400 && expandedBox.height>=960,'the entire editor fills the viewport');
    await page.screenshot({path:'../../.runtime/supplement-editor-fullscreen.png',fullPage:true});
    await page.keyboard.press('Escape');
    await expect(page.getByRole('dialog')).toBeVisible();
    await page.getByRole('button',{name:'关闭窗口'}).click();
    const title='验收文章 '+Date.now();
    const created=await request('/api/admin/articles',{title,category:'教程',body:'# 本地验收\n**真实内容**<script>window.injected=1</script>',published:true,sort:0});
    assert.ok(created.ok(),await created.text());const article=await created.json();
    await page.goto(base+'/#tutorials');
    await page.reload();
    await expect(page.getByText(title,{exact:true})).toBeVisible();
    const attach=await ctx.request.post('/api/admin/articles/'+article.id+'/attachments',{headers:{'X-CSRF-Token':me.csrfToken},multipart:{file:{name:'acceptance.txt',mimeType:'text/plain',buffer:Buffer.from('MSBOOST acceptance')}}});
    assert.ok(attach.ok());const file=await attach.json();
    assert.equal(await (await ctx.request.get(`/api/articles/${article.id}/attachments/${file.id}`)).text(),'MSBOOST acceptance');
    assert.ok((await request('/api/admin/articles/'+article.id,undefined,'DELETE')).ok());
    await page.setViewportSize({width:390,height:844});
    await page.goto(base+'/#home');
    assert.equal(await page.evaluate(()=>document.documentElement.scrollWidth>innerWidth),false,'mobile overflow');
    await page.screenshot({path:'../../.runtime/mobile.png',fullPage:true});
    const member=await browser.newContext({baseURL:base});
    const userEmail=String(Date.now()).slice(-10)+'@qq.com';
    const signup=await request('/api/auth/register',{email:userEmail,password:'Acceptance-Test-Alpha!',agree:true},'POST',member);
    assert.equal(signup.status(),201,await signup.text());
    const user=(await signup.json()).user;assert.ok(!user.passwordHash);assert.equal(user.emailVerifiedAt,0);
    assert.equal((await member.request.get('/api/admin/users')).status(),403);
    const t=await request('/api/tickets',{title:'本地工单验收',body:'仅本地测试，不会联系外部服务'},'POST',member);
    assert.equal(t.status(),201);const ticket=await t.json();
    assert.ok((await request('/api/tickets/'+ticket.id+'/replies',{body:'管理员回复'})).ok());
    assert.ok((await request('/api/tickets/'+ticket.id,{status:'closed'},'PATCH',member)).ok());
    const fingerprints=await request('/api/fingerprints',{host:'127.0.0.1',port:22},'POST',member);
    assert.ok(fingerprints.status()>=400,'private SSH targets must not be probed');
    const mp=await member.newPage();await mp.goto(base+'/#deploy');
    await expect(mp.getByRole('heading',{level:1})).toContainText('部署');
    for (const field of await mp.locator('input[type=password]').all()) await expect(field).toHaveValue('');
    await expect(mp.locator('.side-nav').getByRole('heading',{name:'免费部署工具',exact:true})).toBeVisible();
    await expect(mp.locator('.side-nav').getByRole('heading',{name:'捐赠权益',exact:true})).toBeVisible();
    const cleanup=mp.getByTestId('cleanup-panel');
    await cleanup.locator('summary').filter({hasText:'卸载 MSBOOST'}).click();
    await expect(cleanup.getByRole('button',{name:'继续',exact:true})).toBeVisible();
    await expect(cleanup.getByRole('button',{name:'确认删除',exact:true})).toHaveCount(0);
    await expect(cleanup.getByPlaceholder('输入：确认清理')).toHaveCount(0);
    await expect(cleanup.getByText('清理会停止相关服务并删除清单内配置。',{exact:true})).toBeVisible();
    await expect(cleanup.locator('.notice:visible')).toHaveCount(1);
    const deploymentBox=await mp.locator('.tool-layout > div > .card').first().boundingBox();
    const cleanupBox=await cleanup.boundingBox();
    assert.ok(Math.abs(deploymentBox.x-cleanupBox.x)<1 && Math.abs(deploymentBox.width-cleanupBox.width)<1,'uninstall panel aligns with deployment form');
    await expect(mp.getByText('填写服务器信息即可，提交时会自动核对服务器身份。首次连接会记住这台服务器，以后身份变化会暂停操作。密码会在连接时验证。').first()).toBeVisible();
    await mp.screenshot({path:'../../.runtime/remediation-deploy.png',fullPage:true});

    // UI-only transport fixture. Documentation IPs are never sent to the real
    // task service; both fingerprint requests and cleanup operations are mocked.
    let previews=0, removals=0;
    const previewResults=new Map();
    await mp.route('**/api/fingerprints',async route=>{
      await route.fulfill({json:{fingerprint:'SHA256:'+'A'.repeat(43),rememberedFingerprint:'',authenticationChecked:false}});
    });
    await mp.route('**/api/tasks',async route=>{
      if(route.request().method()!=='POST')return route.continue();
      const input=route.request().postDataJSON();
      if(input.kind==='cleanup-preview'){
        previews++;
        assert.equal(input.cleanup.confirm,false);
        assert.equal(input.cleanup.scope,'msboost');
        const id='ui-preview-'+previews;
        const result={id,kind:'cleanup-preview',host:input.ssh.host,userId:user.id,state:'succeeded',createdAt:Date.now(),cleanup:{scope:'msboost',digest:String(previews).repeat(64),items:[{path:'/etc/msboost',kind:'directory'}],removed:false}};
        previewResults.set(id,result);
        await route.fulfill({json:{...result,state:'queued',cleanup:undefined}});
      }else if(input.kind==='cleanup'){
        removals++;
        assert.equal(previews,2,'editing the host must require another server-side preview');
        assert.equal(input.ssh.host,'203.0.113.11');
        assert.deepEqual(input.cleanup,{scope:'msboost',previewId:'ui-preview-2',digest:'2'.repeat(64),confirm:true});
        await route.fulfill({json:{id:'ui-cleanup-done',kind:'cleanup',host:input.ssh.host,userId:user.id,state:'succeeded',createdAt:Date.now(),cleanup:{...previewResults.get('ui-preview-2').cleanup,removed:true}}});
      }else{
        throw new Error('Unexpected task in isolated cleanup UI fixture');
      }
    });
    await mp.route('**/api/tasks/**',async route=>{
      const id=new URL(route.request().url()).pathname.split('/').pop();
      assert.ok(previewResults.has(id),'only synthetic previews may be polled');
      await route.fulfill({json:previewResults.get(id)});
    });
    await cleanup.getByLabel('服务器 IP 地址',{exact:true}).fill('203.0.113.10');
    await cleanup.getByLabel('SSH 密码').fill('ui-only-password');
    await cleanup.getByRole('button',{name:'继续',exact:true}).click();
    await expect(cleanup.getByRole('button',{name:'确认删除',exact:true})).toBeVisible();
    assert.equal(removals,0,'automatic preflight must not delete anything');
    await expect(cleanup.getByRole('table')).toHaveCount(0);
    await expect(cleanup.getByText('/etc/msboost',{exact:true})).toHaveCount(0);
    await cleanup.getByLabel('服务器 IP 地址',{exact:true}).fill('203.0.113.11');
    await expect(cleanup.getByRole('button',{name:'确认删除',exact:true})).toHaveCount(0);
    await cleanup.getByRole('button',{name:'继续',exact:true}).click();
    await expect(cleanup.getByRole('button',{name:'确认删除',exact:true})).toBeVisible();
    assert.equal(removals,0);
    await cleanup.getByRole('button',{name:'确认删除',exact:true}).click();
    await expect(mp.getByRole('dialog')).toBeVisible();
    assert.equal(removals,1,'exactly one final destructive task follows explicit click');
    await expect(cleanup.getByRole('button',{name:'确认删除',exact:true})).toHaveCount(0);
    await mp.getByRole('button',{name:'关闭窗口'}).click();

    await page.route('**/api/admin/tasks',route=>route.fulfill({json:{tasks:[{id:'ui-fingerprint',kind:'fingerprint',host:'203.0.113.10',state:'succeeded',phase:'fingerprint',createdAt:Date.now()}]}}));
    await page.goto(base+'/#tasks');
    await page.getByRole('button',{name:'刷新',exact:true}).click();
    await expect(page.getByRole('cell').filter({hasText:'验证指纹'})).toBeVisible();
    await expect(page.getByRole('cell',{name:'不生成配置',exact:true})).toBeVisible();
    await member.close();
    assert.deepEqual(errors,[]);
  } finally {await browser.close();}
});
