// A fresh local database per run; never writes to a developer's existing preview data.
import fs from 'node:fs';
import path from 'node:path';
import net from 'node:net';
import crypto from 'node:crypto';
import {spawn} from 'node:child_process';
import {fileURLToPath} from 'node:url';
const root=path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const folder=path.join(root,'.runtime','http-test-'+crypto.randomUUID());
fs.mkdirSync(folder,{recursive:true,mode:0o700});
const credentials=path.join(folder,'credentials.json');
const password=crypto.randomBytes(24).toString('hex').replace(/.{5}/g,'$&-');
fs.writeFileSync(credentials,JSON.stringify({email:'10000001@qq.com',password}),{mode:0o600});
const socket=net.createServer();
await new Promise((resolve,reject)=>{socket.once('error',reject);socket.listen(0,'127.0.0.1',resolve);});
const port=socket.address().port;
await new Promise(resolve=>socket.close(resolve));
const base=`http://127.0.0.1:${port}`;
const binary=process.env.MSBOOST_TEST_BINARY || path.join(root,'.runtime',process.platform==='win32'?'msboost-server.exe':'msboost-server');
const child=spawn(binary,[],{cwd:root,windowsHide:true,stdio:['ignore','pipe','pipe'],env:{...process.env,
 DATABASE_URL:'',DATABASE_HOST:'',DATABASE_USER:'',POSTGRES_PASSWORD:'',MASTER_KEY:'',TRUSTED_PROXY_CIDRS:'',
 WEB_DIR:'apps/web/dist',NODE_SCRIPT:'installers/node/msboost.sh',REINSTALL_SCRIPT:'installers/reinstall/reinstall.sh',REINSTALL_SHA256:'',
 DATA_DIR:folder,PUBLIC_URL:base,LISTEN_ADDR:`127.0.0.1:${port}`,ADMIN_EMAIL:'10000001@qq.com',ADMIN_PASSWORD:password,COOKIE_SECURE:'false'}});
let failure='';
child.stdout.on('data',v=>{failure+=v.toString();});child.stderr.on('data',v=>{failure+=v.toString();});
child.on('error',e=>{failure+=e.message;});
try {
 let ready=false;
 for(let attempt=0;attempt<80;attempt++){
  if(child.exitCode!==null)throw new Error('Isolated server exited: '+failure);
  try {if((await fetch(base+'/api/health')).ok){ready=true;break;}}catch{}
  await new Promise(resolve=>setTimeout(resolve,250));
 }
 if(!ready)throw new Error('Isolated server did not become healthy: '+failure);
 const tests=process.argv.includes('--browser') ? ['tests/acceptance.test.mjs','tests/articles.test.mjs','tests/payment-return.test.mjs','tests/password-reset.test.mjs','tests/supplement23-copy.test.mjs','tests/relay-status.test.mjs','tests/plans-policy.test.mjs','tests/tunnels.test.mjs'] : ['tests/http.test.mjs','tests/users-model.test.mjs','tests/navigation.test.mjs'];
 const test=spawn(process.execPath,['--test',...tests],{cwd:path.join(root,'apps/web'),stdio:'inherit',windowsHide:true,env:{...process.env,MSBOOST_TEST_URL:base,MSBOOST_TEST_CREDENTIALS:credentials}});
 process.exitCode=await new Promise((resolve,reject)=>{test.once('error',reject);test.once('exit',code=>resolve(code??1));});
} finally {
 child.kill();
 console.log('Isolated HTTP test data retained under .runtime for inspection; existing preview data was not modified.');
}
