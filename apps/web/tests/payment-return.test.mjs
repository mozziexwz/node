import { test } from 'node:test';
import assert from 'node:assert/strict';
import { chromium, expect } from '@playwright/test';
import { build } from 'esbuild';
import { fileURLToPath } from 'node:url';

// Static UI + synthetic intercepted API responses only. Never create a real
// merchant transaction, use real credentials, or follow an external gateway.
const base = process.env.MSBOOST_TEST_URL || 'http://127.0.0.1:8080';
assert.match(base, /^http:\/\/(127\.0\.0\.1|localhost):\d+$/);
const sample = {id:'synthetic-order_123',userId:'buyer',plan:{name:'合成测试套餐'},amountCents:1234,channelId:'synthetic-channel',state:'pending'};
const detailName = count => `${sample.plan.name}（查询响应 ${count}）`;

test('payment return: authenticated status, bounded polling, login continuation and readonly channel URLs', {timeout:60000}, async t => {
  const browser = await chromium.launch({headless:true,...(process.platform==='win32'?{channel:'chrome'}:{})});
  async function fixture({signedIn=true,admin=false,state='pending',orderStatus=200}={}) {
    const page=await browser.newPage({baseURL:base});
    await page.clock.install();
    const requests=[],errors=[];
    const model={signedIn,state,orderStatus,details:0,me:0,hang:false,deferDetails:false,pendingDetails:[]};
    page.on('pageerror',e=>errors.push(e.message));
    await page.route('**/*',async route=>{
      const url=new URL(route.request().url());
      if(url.origin!==base) return route.abort();
      if(!url.pathname.startsWith('/api/')) return route.continue();
      const path=url.pathname,method=route.request().method();
      requests.push({path,method});
      const user={id:'buyer',email:'10000001@qq.com',role:admin?'admin':'member',status:'active',balanceCents:9876,expiresAt:0};
      if(path==='/api/auth/login') {model.signedIn=true;return route.fulfill({json:{user}});}
      if(path==='/api/me') {model.me++;return route.fulfill(model.signedIn?{json:{user}}:{status:401,json:{error:'未登录'}});}
      if(path===`/api/orders/${sample.id}`) {
        model.details++;
        if(model.hang) return;
        const response=model.orderStatus===200?{json:{...sample,plan:{...sample.plan,name:detailName(model.details)},state:model.state,paymentUrl:'https://gateway.invalid/synthetic-checkout',reviewReason:model.state==='paid_review'?'付款通知超过订单有效期':undefined}}:{status:model.orderStatus,json:{error:'订单不存在'}};
        if(model.deferDetails) {model.pendingDetails.push(()=>route.fulfill(response));return;}
        return route.fulfill(response);
      }
      if(path==='/api/plans') return route.fulfill({json:{plans:[{id:'synthetic-plan',name:'合成测试套餐',priceCents:1234,days:1,trafficBytes:1000000000,enabled:true}]}});
      if(path==='/api/payment-channels') return route.fulfill({json:{channels:[{id:'synthetic-channel',name:'合成渠道'}]}});
      if(path==='/api/orders'&&method==='POST') {
        const {channelId}=route.request().postDataJSON();
        return route.fulfill({status:201,json:{...sample,channelId,state:channelId==='balance'?'paid':'pending',paymentUrl:channelId==='balance'?undefined:'https://gateway.invalid/synthetic-checkout'}});
      }
      if(path==='/api/orders'||path==='/api/admin/orders') return route.fulfill({json:{orders:[]}});
      if(path==='/api/admin/payment-channels') return route.fulfill({json:{channels:[{id:'synthetic-channel',name:'合成渠道',type:'alipay',version:'v2',gateway:'https://gateway.invalid',merchantId:'synthetic',enabled:true,configured:true,notifyUrl:'https://shop.example/api/payments/epay/synthetic-channel/notify',returnUrlTemplate:'https://shop.example/?order={orderId}'}]}});
      return route.fulfill({json:{}});
    });
    return {page,model,requests,errors};
  }
  async function detailSettled(page,model,count) {
    await expect.poll(()=>model.details).toBe(count);
    const panel=page.getByRole('region',{name:'支付返回结果'});
    // Request arrival is not response.json completion. The next 5s timer is
    // installed only after this particular result is consumed. Waiting for
    // its unique rendered value and finally/busy state prevents advancing the
    // fake clock while a slow intercepted response is still in flight.
    await expect(panel).toContainText(detailName(count));
    await expect(panel.getByRole('button',{name:'刷新付款状态',exact:true})).toBeEnabled();
  }
  try {
    await t.test('component reused without a key isolates a new order and refreshes each paid order',async()=>{
      const compiled=await build({stdin:{contents:`
        import React from 'react';
        import {createRoot} from 'react-dom/client';
        import {PaymentReturn} from './payment-return';
        const root=createRoot(document.getElementById('fixture'));
        window.paymentSignals=[];
        window.renderPaymentOrder=id=>root.render(React.createElement(PaymentReturn,{orderID:id,onPaid:()=>window.paymentSignals.push(id)}));
      `,resolveDir:fileURLToPath(new URL('../src/',import.meta.url)),loader:'jsx'},bundle:true,write:false,format:'iife',platform:'browser'});
      const page=await browser.newPage({baseURL:base});
      let secondRequest;
      await page.route('**/*',route=>{
        const url=new URL(route.request().url());
        if(url.origin!==base) return route.abort();
        if(url.pathname==='/component-harness') return route.fulfill({contentType:'text/html',body:'<div id="fixture"></div>'});
        if(url.pathname==='/api/orders/order-A') return route.fulfill({json:{...sample,id:'order-A',state:'paid',plan:{name:'第一个订单'}}});
        if(url.pathname==='/api/orders/order-B') {secondRequest=route;return;}
        return route.abort();
      });
      try {
        await page.goto('/component-harness');
        await page.addScriptTag({content:compiled.outputFiles[0].text});
        await page.evaluate(()=>window.renderPaymentOrder('order-A'));
        await expect(page.getByRole('region',{name:'支付返回结果'})).toContainText('第一个订单');
        assert.deepEqual(await page.evaluate(()=>window.paymentSignals),['order-A']);
        await page.evaluate(()=>window.renderPaymentOrder('order-B'));
        const panel=page.getByRole('region',{name:'支付返回结果'});
        await expect(panel).toContainText('订单号：order-B');
        await expect(panel).not.toContainText('第一个订单');
        await expect(panel).not.toContainText('已支付，权益已生效');
        await expect.poll(()=>!!secondRequest).toBe(true);
        await secondRequest.fulfill({json:{...sample,id:'order-B',state:'paid',plan:{name:'第二个订单'}}});
        await expect(panel).toContainText('第二个订单');
        assert.deepEqual(await page.evaluate(()=>window.paymentSignals),['order-A','order-B']);
      } finally {await page.close();}
    });
    await t.test('forged success ignored; 13 reads maximum; manual refresh confirms only server paid',async()=>{
      const {page,model,requests,errors}=await fixture();
      try {
        await page.goto(`/?order=${sample.id}&trade_status=TRADE_SUCCESS&state=paid&sign=untrusted`);
        const panel=page.getByRole('region',{name:'支付返回结果'});
        await expect(panel).toContainText('等待付款通知');
        await expect(panel).not.toContainText('已支付，权益已生效');
        await detailSettled(page,model,1);
        for(let i=2;i<=13;i++) {
          await page.clock.runFor(5000);
          await detailSettled(page,model,i);
        }
        await expect(panel).toContainText('本轮自动查询已结束');
        await page.clock.runFor(30000);
        assert.equal(model.details,13);
        model.state='paid';
        const meBefore=model.me;
        await panel.getByRole('button',{name:'刷新付款状态',exact:true}).click();
        await expect(panel).toContainText('已支付，权益已生效');
        await expect.poll(()=>model.me).toBeGreaterThan(meBefore);
        await page.clock.runFor(30000);
        assert.equal(model.details,14);
        assert.ok(requests.every(r=>r.method==='GET'));
        assert.ok(!requests.some(r=>r.path.includes('/notify')));
        assert.deepEqual(errors,[]);
      } finally {await page.close();}
    });
    await t.test('delayed response does not overlap reads; next poll starts after that response settles',async()=>{
      const {page,model,requests,errors}=await fixture();
      try {
        model.deferDetails=true;
        await page.goto(`/?order=${sample.id}&state=paid`);
        const panel=page.getByRole('region',{name:'支付返回结果'});
        await expect.poll(()=>model.details).toBe(1);
        await expect(panel.getByRole('button',{name:'正在查询…',exact:true})).toBeDisabled();
        await page.clock.runFor(5000);
        assert.equal(model.details,1);
        assert.equal(model.pendingDetails.length,1);
        await expect(panel).not.toContainText(detailName(1));
        await expect(panel).not.toContainText('已支付，权益已生效');
        model.deferDetails=false;
        await model.pendingDetails.shift()();
        await detailSettled(page,model,1);
        await page.clock.runFor(5000);
        await detailSettled(page,model,2);
        await expect(panel).toContainText('等待付款通知');
        assert.ok(requests.every(r=>r.method==='GET'));
        assert.deepEqual(errors,[]);
      } finally {await page.close();}
    });
    await t.test('login keeps order destination; does not query orders before authentication',async()=>{
      const {page,model,requests}=await fixture({signedIn:false,state:'paid'});
      try {
        await page.goto(`/?order=${sample.id}`);
        await expect(page.getByLabel('QQ 邮箱')).toBeVisible();
        assert.equal(model.details,0);
        await page.getByLabel('QQ 邮箱').fill('10000001@qq.com');
        await page.getByLabel('密码',{exact:true}).fill('synthetic-local-only');
        await page.getByRole('checkbox').check();
        await page.locator('form').getByRole('button',{name:'登录',exact:true}).click();
        await expect(page).toHaveURL(new RegExp(`\\?order=${sample.id}#orders$`));
        await expect(page.getByRole('region',{name:'支付返回结果'})).toContainText('已支付，权益已生效');
        assert.deepEqual(requests.filter(r=>r.method!=='GET').map(r=>r.path),['/api/auth/login']);
      } finally {await page.close();}
    });
    for(const state of ['paid_review','expired']) await t.test(`${state} stays distinct and stops automatic polling`,async()=>{
      const {page,model}=await fixture({state});
      try {
        await page.goto(`/?order=${sample.id}`);
        const panel=page.getByRole('region',{name:'支付返回结果'});
        await expect(panel).toContainText(state==='paid_review'?'款项已收到，待人工核对':'订单已过期');
        await expect(panel).toContainText('请勿重复付款');
        await expect(panel).not.toContainText('已支付，权益已生效');
        await page.clock.runFor(70000);
        assert.equal(model.details,1);
      } finally {await page.close();}
    });
    await t.test('foreign/missing order displays only authorized API error',async()=>{
      const {page,model}=await fixture({orderStatus:404});
      try {
        await page.goto(`/?order=${sample.id}&state=paid`);
        const panel=page.getByRole('region',{name:'支付返回结果'});
        await expect(panel).toContainText('订单不存在');
        await expect(panel).not.toContainText('合成测试套餐');
        await page.clock.runFor(70000);
        assert.equal(model.details,1);
      } finally {await page.close();}
    });
    await t.test('stalled request times out and manual refresh remains available',async()=>{
      const {page,model}=await fixture();
      try {
        model.hang=true;
        await page.goto(`/?order=${sample.id}`);
        await expect.poll(()=>model.details).toBe(1);
        await page.clock.runFor(10000);
        const panel=page.getByRole('region',{name:'支付返回结果'});
        await expect(panel).toContainText('订单查询超时，请手动刷新');
        model.hang=false;
        await panel.getByRole('button',{name:'刷新付款状态',exact:true}).click();
        await expect(panel).toContainText('等待付款通知');
      } finally {await page.close();}
    });
    for(const channelId of ['synthetic-channel','balance']) await t.test(`created ${channelId} order preserves correct payment feedback`,async()=>{
      const {page,model,requests}=await fixture();
      try {
        await page.goto('/#plans');
        await page.getByRole('button',{name:'选择套餐',exact:true}).click();
        await page.getByLabel('支付方式').selectOption(channelId);
        await page.getByLabel('我理解新套餐将覆盖旧的剩余时间和流量').check();
        await page.getByRole('button',{name:'确认购买',exact:true}).click();
        const dialog=page.getByRole('dialog');
        if(channelId==='balance') {
          await expect(dialog).toContainText('已支付，权益已生效');
          assert.equal(model.details,0);
        } else {
          await expect(dialog).toContainText('等待付款通知');
          await expect(dialog.getByRole('link',{name:'前往支付',exact:true})).toBeVisible();
          model.state='paid';
          await dialog.getByRole('button',{name:'刷新付款状态',exact:true}).click();
          await expect(dialog).toContainText('已支付，权益已生效');
          await expect(dialog.getByRole('link',{name:'前往支付',exact:true})).toHaveCount(0);
        }
        assert.deepEqual(requests.filter(r=>r.method!=='GET').map(r=>r.path),['/api/orders']);
      } finally {await page.close();}
    });
    await t.test('malformed or repeated order selectors never construct an API path',async()=>{
      const {page,requests}=await fixture();
      try {
        for(const query of ['?order=..%2Fadmin%2Forders','?order=a&order=b','?order=https%3A%2F%2Fevil.invalid']) {
          await page.goto('/'+query);
          await expect(page.locator('.sidebar')).toBeVisible();
          await expect(page.getByRole('region',{name:'支付返回结果'})).toHaveCount(0);
        }
        assert.ok(!requests.some(r=>r.path.startsWith('/api/orders')));
      } finally {await page.close();}
    });
    await t.test('admin can inspect generated callback and return template without editing them',async()=>{
      const {page}=await fixture({admin:true});
      try {
        await page.goto('/#payments');
        await expect(page.locator('main')).toContainText('每次下单自动提交，无需手填');
        await page.getByRole('button',{name:'编辑',exact:true}).click();
        const notify=page.getByLabel('通知地址 / 异步回调地址（自动生成）');
        const returning=page.getByLabel('返回地址 / 同步跳转入口（模板）');
        await expect(notify).toHaveValue('https://shop.example/api/payments/epay/synthetic-channel/notify');
        await expect(returning).toHaveValue('https://shop.example/?order={orderId}');
        assert.equal(await notify.evaluate(el=>el.readOnly),true);
        assert.equal(await returning.evaluate(el=>el.readOnly),true);
      } finally {await page.close();}
    });
  } finally {await browser.close();}
});
