import {test} from 'node:test';
import assert from 'node:assert/strict';
import fs from 'node:fs';
import crypto from 'node:crypto';

const base=process.env.MSBOOST_TEST_URL||'http://127.0.0.1:8080';
assert.match(base,/^http:\/\/(127\.0\.0\.1|localhost):\d+$/,'Acceptance tests only target isolated localhost');
function client(){
  let cookie='',csrf='';
  return async(path,data,method=data===undefined?'GET':'POST',extra={})=>{
    const headers={Origin:base,...(cookie?{Cookie:cookie}:{}),...(csrf?{'X-CSRF-Token':csrf}:{}),...extra};
    if(data!==undefined)headers['Content-Type']='application/json';
    const res=await fetch(base+path,{method,headers,body:data===undefined?undefined:JSON.stringify(data)});
    const set=res.headers.getSetCookie();if(set.length)cookie=set[0].split(';')[0];
    const body=await res.json();if(body.csrfToken)csrf=body.csrfToken;
    return {status:res.status,body};
  };
}
test('HTTP integration: real auth, admin CRUD, financial idempotency and permission gates',{timeout:30000},async()=>{
  const admin=client(),member=client();
  const creds=JSON.parse(fs.readFileSync(process.env.MSBOOST_TEST_CREDENTIALS || '../../.runtime/acceptance-credentials.json','utf8'));
  const login=await admin('/api/auth/login',creds);
  assert.equal(login.status,200,JSON.stringify(login.body));assert.equal(login.body.user.role,'admin');assert.ok(!login.body.user.passwordHash);
  for(const endpoint of ['users','plans','cards','orders','invitations','settings','tasks','executors','relay-agents','routes','user-rules','articles','overview','backups','backup-plan','backup-targets']){
    const r=await admin('/api/admin/'+endpoint);assert.equal(r.status,200,endpoint+':'+JSON.stringify(r.body));
  }
  const noCSRF=await admin('/api/admin/plans',{},'POST',{'X-CSRF-Token':''});assert.equal(noCSRF.status,403);
  const guard=await admin('/api/admin/settings',{registrationEmailVerificationRequired:true},'PUT');assert.equal(guard.status,400,'SMTP readiness required');
  const invite=await admin('/api/admin/invitations',{count:2,maxUses:1,batch:'acceptance'});assert.equal(invite.status,201,JSON.stringify(invite.body));
  const ids=invite.body.invitations.map(i=>i.id);
  assert.equal((await admin('/api/admin/invitations/batch',{ids,action:'disable'})).status,200);
  const title='API验收 '+Date.now();
  const article=await admin('/api/admin/articles',{title,body:'# 本地文章',published:true,sort:0});assert.equal(article.status,200);
  const publicArticles=await member('/api/articles');assert.ok(publicArticles.body.articles.some(a=>a.id===article.body.id));
  assert.equal((await admin('/api/admin/articles/'+article.body.id,undefined,'DELETE')).status,200);
  const signup=await member('/api/auth/register',{email:String(Date.now()).slice(-10)+'@qq.com',password:'Local-Acceptance-Alpha!',agree:true});
  assert.equal(signup.status,201,JSON.stringify(signup.body));assert.equal(signup.body.user.emailVerifiedAt,0);assert.ok(!signup.body.user.passwordHash);
  assert.equal((await member('/api/admin/users')).status,403);
  const card=await admin('/api/admin/cards',{count:1,amountCents:1000,batch:'local-acceptance'});assert.equal(card.status,201,JSON.stringify(card.body));
  const redeem={code:card.body.cards[0].code,requestId:crypto.randomUUID()};
  const redemptions=await Promise.all([member('/api/wallet/redeem',redeem),member('/api/wallet/redeem',redeem)]);
  assert.ok(redemptions.every(r=>r.status===200));assert.equal((await member('/api/wallet')).body.balanceCents,1000);
  const p=await admin('/api/admin/plans',{name:'本地验收套餐',priceCents:100,days:1,trafficBytes:1000000000,rateMbps:1,enabled:true});
  assert.ok(p.status<300,JSON.stringify(p.body));const plan=p.body.plan||p.body;
  const buy={planId:plan.id,channelId:'balance',requestId:crypto.randomUUID(),confirmReplace:true};
  const order=await member('/api/orders',buy);assert.ok(order.status<300,JSON.stringify(order.body));
  const again=await member('/api/orders',buy);assert.ok(again.status<300);assert.equal((await member('/api/wallet')).body.balanceCents,900);
  assert.equal((await member('/api/me')).body.user.trafficTotal,1000000000);
  const ticket=await member('/api/tickets',{title:'API验收工单',body:'仅本地验收'});assert.equal(ticket.status,201);
  assert.equal((await admin('/api/tickets/'+ticket.body.id+'/replies',{body:'已收到'})).status,200);
  assert.equal((await member('/api/tickets/'+ticket.body.id,{status:'closed'},'PATCH')).status,200);
  const ssh=await member('/api/fingerprints',{host:'127.0.0.1',port:22});assert.ok(ssh.status>=400&&ssh.status!==404,'reject private SSH endpoint');
  const backup=await admin('/api/admin/backups',{});assert.ok(backup.status<300,JSON.stringify(backup.body));
  assert.equal((await admin('/api/admin/users')).body.users.some(u=>u.passwordHash),false);
});
