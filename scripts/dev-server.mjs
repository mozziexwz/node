// Isolated local acceptance environment. Credentials never enter source control.
import fs from 'node:fs';
import crypto from 'node:crypto';
import {spawn} from 'node:child_process';
fs.mkdirSync('.runtime',{recursive:true});
const file='.runtime/acceptance-credentials.json';
const credentials=fs.existsSync(file)?JSON.parse(fs.readFileSync(file,'utf8')):{email:'10000001@qq.com',password:crypto.randomBytes(24).toString('base64url')};
fs.writeFileSync(file,JSON.stringify(credentials),{mode:0o600});
const child=spawn(process.platform==='win32'?'.runtime/msboost-server.exe':'.runtime/msboost-server',[],{env:{...process.env,DATABASE_URL:'',DATABASE_HOST:'',DATABASE_USER:'',POSTGRES_PASSWORD:'',MASTER_KEY:'',TRUSTED_PROXY_CIDRS:'',WEB_DIR:'apps/web/dist',NODE_SCRIPT:'installers/node/msboost.sh',REINSTALL_SCRIPT:'installers/reinstall/reinstall.sh',REINSTALL_SHA256:'',DATA_DIR:'.runtime/acceptance-data',PUBLIC_URL:'http://127.0.0.1:8080',LISTEN_ADDR:'127.0.0.1:8080',ADMIN_EMAIL:credentials.email,ADMIN_PASSWORD:credentials.password,COOKIE_SECURE:'false'},stdio:'inherit',windowsHide:true});
console.log('Local acceptance server starting on http://127.0.0.1:8080 (isolated test data).');
child.on('exit',code=>process.exit(code??0));
for(const signal of ['SIGINT','SIGTERM'])process.on(signal,()=>child.kill(signal));
